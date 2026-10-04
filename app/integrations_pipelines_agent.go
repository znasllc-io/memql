//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelinerun"
)

// integrations_pipelines_agent.go -- the agent node DRIVES pipeline runs
// (epic memql#5477, plan decision 10).
//
// A run is opened wherever its cause arrives (wirePipelines, on every node):
// the bff for a webhook, the agent replica the poll is placed on. Driving it
// -- claiming it under the row lease, compiling it into the work spine,
// handing its steps to the runner, reporting the check run -- happens here,
// on agent nodes only, for the reason the work run dispatcher's does
// (integrations_work_dispatch.go): the agent node is where steps execute, and
// where the substrate registers its step executor (epic memql#5478).
//
// THE EVENTS ARE THE TRIGGER, THE POLL IS THE BACKSTOP. Every v1:pipelines:run
// write is broadcast to every node (component/node/routing.go), so each agent
// replica hears a queued run the moment it is opened, and the gated claim
// lets exactly one of them drive it. The every-minute poll -- placed on one
// agent replica behind its cron lease -- calls the driver's recovery, which
// takes up a run nobody claimed and one whose driver went silent.
func (a *App) wirePipelinesDriver() {
	integ := a.lookupPipelinesIntegration()
	if integ == nil {
		a.Logger.Warn("pipelines driver not wired: the pipelines plug-in did not materialize on this agent node; queued runs will never start",
			"component", "pipelinerun")
		return
	}
	if a.eventBus == nil {
		a.Logger.Warn("pipelines driver not wired: no event bus on this agent node; runs start only when the poll recovers them",
			"component", "pipelinerun")
	}

	// The driver itself, plus the two hooks the opening half reaches it
	// through: the poll's recovery and a cancel's signal.
	integ.EnableDriver()

	// created AND updated, as the work dispatcher subscribes: a run is
	// created queued, and a cancel request arrives as an update.
	if a.eventBus != nil {
		for _, topic := range []string{
			events.BuildTopicWithConcept(events.TopicGraphNodeCreated, pipelinerun.RunConcept),
			events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, pipelinerun.RunConcept),
		} {
			a.eventBus.Subscribe(topic, integ.HandleRunEvent,
				events.WithSubscriberName("pipelines:run-driver"))
		}
	}

	a.Logger.Info("pipelines driver wired: this node claims, drives and recovers pipeline runs",
		"component", "pipelinerun")
}
