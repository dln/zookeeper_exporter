package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"
)

type Servers struct {
	Servers []string `json:"servers"`
	Port    int      `json:"port"`
}

const concurrentFetch = 100

const (
	follower = iota
	leader
)

// Commandline flags.
var (
	useExhibitor = flag.Bool("exporter.use_exhibitor", false, "Use Exhibitor to discover ZooKeeper servers")
	addr         = flag.String("web.listen-address", ":9114", "Address to listen on for web interface and telemetry.")
	metricPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
)

var (
	variableLabels = []string{"server"}

	latencyMin = prometheus.NewDesc(
		"zookeeper_latency_min_ms",
		"Minimum latency.",
		variableLabels, nil,
	)
	latencyAvg = prometheus.NewDesc(
		"zookeeper_latency_avg_ms",
		"Average latency.",
		variableLabels, nil,
	)
	latencyMax = prometheus.NewDesc(
		"zookeeper_latency_max_ms",
		"Maximum latency.",
		variableLabels, nil,
	)
	received = prometheus.NewDesc(
		"zookeeper_packets_received",
		"Bytes received.",
		variableLabels, nil,
	)
	sent = prometheus.NewDesc(
		"zookeeper_packets_sent",
		"Bytes received.",
		variableLabels, nil,
	)
	connections = prometheus.NewDesc(
		"zookeeper_connections",
		"Alive connections.",
		variableLabels, nil,
	)
	outstanding = prometheus.NewDesc(
		"zookeeper_outstanding_requests",
		"Outstanding requests.",
		variableLabels, nil,
	)
	mode = prometheus.NewDesc(
		"zookeeper_leader",
		"Host is leader.",
		variableLabels, nil,
	)
	znodeCount = prometheus.NewDesc(
		"zookeeper_znode_count",
		"Number of znodes in tree.",
		variableLabels, nil,
	)
)

var httpClient = http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 10 * time.Second,
		Dial: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
		}).Dial,
	},
}

type exporter struct {
	sync.Mutex
	addrs        []string
	useExhibitor bool
	errors       prometheus.Counter
	duration     prometheus.Gauge
}

func newZooKeeperExporter(addrs []string, useExhibitor bool) *exporter {
	e := &exporter{
		addrs:        addrs,
		useExhibitor: useExhibitor,
		errors: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: "zookeeper_exporter",
				Name:      "exporter_scrape_errors_total",
				Help:      "Current total scrape errors",
			},
		),
		duration: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "zookeeper_exporter",
				Name:      "exporter_last_scrape_duration_ms",
				Help:      "Last scrape duration in milliseconds",
			},
		),
	}
	return e
}

func (e *exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.duration.Desc()
	ch <- e.errors.Desc()
}

func (e *exporter) Collect(ch chan<- prometheus.Metric) {
	e.Lock()
	defer e.Unlock()

	metricsChan := make(chan prometheus.Metric)
	go e.scrape(metricsChan)

	for metric := range metricsChan {
		ch <- metric
	}

	ch <- e.errors
	ch <- e.duration
}

func (e *exporter) recordErr(err error) {
	log.Error("Error: ", err)
	e.errors.Inc()
}

func (e *exporter) scrape(ch chan<- prometheus.Metric) {
	defer close(ch)

	servers := []string{}

	if e.useExhibitor {
		url := fmt.Sprintf("http://%s/exhibitor/v1/cluster/list", e.addrs[0])
		rr, err := http.NewRequest("GET", url, nil)
		if err != nil {
			panic(err)
		}

		rresp, err := httpClient.Transport.RoundTrip(rr)
		if err != nil {
			e.recordErr(err)
			return
		}
		defer rresp.Body.Close()

		body, err := ioutil.ReadAll(rresp.Body)
		if err != nil {
			e.recordErr(err)
			return
		}

		var serverList Servers
		err = json.Unmarshal(body, &serverList)
		if err != nil {
			e.recordErr(err)
			return
		}

		log.Debugf("Got serverlist from Exhibitor: %s", serverList)

		for _, host := range serverList.Servers {
			servers = append(servers, fmt.Sprintf("%s:%d", host, serverList.Port))
		}
	} else {
		servers = e.addrs
	}

	log.Debugf("Polling servers: %s", servers)
	var wg sync.WaitGroup
	for _, server := range servers {
		log.Debugf("Polling server: %s", server)
		wg.Add(1)
		go e.pollServer(server, ch, &wg)
	}
	wg.Wait()
}

func (e *exporter) pollServer(server string, ch chan<- prometheus.Metric, wg *sync.WaitGroup) {
	defer wg.Done()

	conn, err := net.Dial("tcp", server)
	if err != nil {
		e.recordErr(err)
		return
	}

	fmt.Fprintf(conn, "stat\n")

	scanner := bufio.NewScanner(conn)

	sepFound := false
	i := 0
	for scanner.Scan() {
		entry := scanner.Text()
		if sepFound {
			i = i + 1
		} else {
			if len(entry) < 2 {
				sepFound = true
			}
			continue
		}
		s := strings.Split(entry, ": ")[1]
		switch i {
		case 1: // Latencies
			lat := strings.Split(s, "/")
			min, _ := strconv.Atoi(lat[0])
			avg, _ := strconv.Atoi(lat[1])
			max, _ := strconv.Atoi(lat[2])
			log.Debugf("Latency:  min=%d avg=%d max=%d", min, avg, max)
			ch <- prometheus.MustNewConstMetric(latencyMin, prometheus.GaugeValue, float64(min), server)
			ch <- prometheus.MustNewConstMetric(latencyAvg, prometheus.GaugeValue, float64(avg), server)
			ch <- prometheus.MustNewConstMetric(latencyMax, prometheus.GaugeValue, float64(max), server)
		case 2: // Received
			v, _ := strconv.Atoi(s)
			log.Debugf("Received: %d", v)
			ch <- prometheus.MustNewConstMetric(received, prometheus.GaugeValue, float64(v), server)
		case 3: // Sent
			v, _ := strconv.Atoi(s)
			log.Debugf("Sent: %d", v)
			ch <- prometheus.MustNewConstMetric(sent, prometheus.GaugeValue, float64(v), server)
		case 4: // Connections
			v, _ := strconv.Atoi(s)
			log.Debugf("Connections: %d", v)
			ch <- prometheus.MustNewConstMetric(connections, prometheus.GaugeValue, float64(v), server)
		case 5: // Outstanding
			v, _ := strconv.Atoi(s)
			log.Debugf("Outstanding: %d", v)
			ch <- prometheus.MustNewConstMetric(outstanding, prometheus.GaugeValue, float64(v), server)
		case 6: // Zkid
		case 7: // Mode
			var v float64
			if s == "leader" {
				v = leader
			} else {
				v = follower
			}
			log.Debugf("Mode: %d", v)
			ch <- prometheus.MustNewConstMetric(mode, prometheus.GaugeValue, v, server)
		case 8: // Node Count
			v, _ := strconv.Atoi(s)
			log.Debugf("Node count: %d", v)
			ch <- prometheus.MustNewConstMetric(znodeCount, prometheus.GaugeValue, float64(v), server)
			break
		}
	}
}

func main() {
	flag.Parse()
	exporter := newZooKeeperExporter(flag.Args(), *useExhibitor)
	prometheus.MustRegister(exporter)

	http.Handle(*metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, *metricPath, http.StatusMovedPermanently)
	})

	log.Info("starting mesos_exporter on ", *addr)

	log.Fatal(http.ListenAndServe(*addr, nil))
}
