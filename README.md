# Prometheus ZooKeeper Exporter

An exporter for [Prometheus](http://prometheus.io/) to monitor an [Apache ZooKeeper](http://zookeeper.apache.org/) ensemble.

## Building and Running

    make
    ./zookeeper_exporter

### Modes

zookeeper_exporter can be run in two modes: exhibitor and explicit.

In explicit mode (the default), it monitors a list of ZooKeeper servers given as command line args.

In exhibitor mode, the exporter will automatically discover servers by querying [Exhibitor]. Command lines args are assumed to be a list of Exhibitor nodes. To enable exhibitor mode, use the `-exporter.discovery.exhibitor` flag.

### Running via docker

An example run uzing docker:

    docker run -d -p 9114:9114 bergerx/zookeeper_exporter zkserver1:2181 zkserver2:2181 zkserver3:2181
