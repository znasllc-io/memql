package metrics

import "github.com/prometheus/client_golang/prometheus"

var wsQueryRejected = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: namespace,
	Subsystem: "ws",
	Name:      "query_rejected_total",
	Help:      "Queries refused because a browser connection's waiting queue is full. A rising count indicates client request pressure or slow query completion.",
})

func WSQueryRejected() { wsQueryRejected.Inc() }

func WSQueryRejectedValue() float64 { return counterValue(wsQueryRejected) }
