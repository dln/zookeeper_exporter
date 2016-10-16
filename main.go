package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"regexp"
	"strconv"
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

// Commandline flags.
var (
	useExhibitor = flag.Bool("exporter.use_exhibitor", false, "Use Exhibitor to discover ZooKeeper servers")
	addr         = flag.String("web.listen-address", ":9114", "Address to listen on for web interface and telemetry.")
	metricPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
)

var (
	variableLabels = []string{"server"}
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

	fmt.Fprintf(conn, "mntr\n")

	scanner := bufio.NewScanner(conn)

	r := regexp.MustCompile("[^\\s]+")
	for scanner.Scan() {
		entry := scanner.Text()

		key := r.FindAllString(entry, -1)[0]
		value := r.FindAllString(entry, -1)[1]

		switch key {
		case "zk_version":
			continue
		case "zk_server_state":
			log.Debugf("%s: %d", key+"_"+value, 1)
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					key+"_"+value,
					"Server State.",
					variableLabels, nil,
				), prometheus.GaugeValue, 1, server)
		default:
			v, _ := strconv.Atoi(value)
			log.Debugf("%s: %d", key, v)
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					key,
					key,
					variableLabels, nil,
				), prometheus.GaugeValue, float64(v), server)
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

	log.Info("starting zookeeper_exporter on ", *addr)

	log.Fatal(http.ListenAndServe(*addr, nil))
}
