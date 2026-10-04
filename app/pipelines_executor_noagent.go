//go:build !agent

package app

import "github.com/znasllc-io/memql/component/node"

// wirePipelinesExecutor registers nothing off the agent node: the pipelines
// driver calls its executor on agent nodes only (epic memql#5477), and the
// fleet half dispatches through the worker streams that terminate there.
func (a *App) wirePipelinesExecutor(_ *node.Identity) {}
