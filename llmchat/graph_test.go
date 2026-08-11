package llmchat

import (
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adk_session "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
)

// stubAgent 构造一个空操作 agent，用于在没有模型的情况下测试图的连线。
type stubAgent struct {
	name string
}

func (s *stubAgent) Build(model.LLM) (agent.Agent, error) {
	return agent.New(agent.Config{
		Name: s.name,
		Run: func(agent.InvocationContext) iter.Seq2[*adk_session.Event, error] {
			return func(func(*adk_session.Event, error) bool) {}
		},
	})
}

type failingAgent struct{}

func (f *failingAgent) Build(model.LLM) (agent.Agent, error) {
	return nil, errors.New("boom")
}

func noopNode(name string) workflow.Node {
	return workflow.NewFunctionNode(name, func(agent.Context, any) (any, error) {
		return nil, nil
	}, workflow.NodeConfig{})
}

func TestGraphAgentBuild(t *testing.T) {
	ga := &GraphAgent{
		Name: "pipeline",
		Edges: func(g *Graph) []workflow.Edge {
			draft := g.Agent(&stubAgent{name: "draft"}, workflow.NodeConfig{})
			return workflow.Chain(workflow.Start, draft, noopNode("publish"))
		},
	}

	got, err := ga.Build(nil)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	if got.Name() != "pipeline" {
		t.Errorf("Name() = %q, want %q", got.Name(), "pipeline")
	}
	if len(got.SubAgents()) != 1 {
		t.Errorf("SubAgents() len = %d, want 1 (agent nodes must be registered)", len(got.SubAgents()))
	}
}

func TestGraphAgentBuildMissingEdges(t *testing.T) {
	if _, err := (&GraphAgent{Name: "pipeline"}).Build(nil); err == nil {
		t.Fatal("Build() error = nil, want an error when Edges is unset")
	}
}

func TestGraphAgentBuildNodeError(t *testing.T) {
	ga := &GraphAgent{
		Name: "pipeline",
		Edges: func(g *Graph) []workflow.Edge {
			// 建节点失败后仍要能继续连边（拿到的是占位节点），错误在 Build 时才报出
			bad := g.Agent(&failingAgent{}, workflow.NodeConfig{})
			return workflow.Chain(workflow.Start, bad, noopNode("next"))
		},
	}

	_, err := ga.Build(nil)
	if err == nil {
		t.Fatal("Build() error = nil, want the deferred node error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Build() error = %v, want it to wrap the builder error", err)
	}
}

func TestGraphAgentBuildInvalidGraph(t *testing.T) {
	ga := &GraphAgent{
		Name: "pipeline",
		Edges: func(g *Graph) []workflow.Edge {
			// 没有从 Start 出发的边
			return workflow.Chain(noopNode("a"), noopNode("b"))
		},
	}

	if _, err := ga.Build(nil); !errors.Is(err, workflow.ErrNoStartNode) {
		t.Fatalf("Build() error = %v, want ErrNoStartNode", err)
	}
}

// TestGraphAgentBuildBranchingTopology 覆盖条件分支、退回环与 JoinNode 扇入，
// 这几种拓扑都会在 workflow.New 的校验期被检查。
func TestGraphAgentBuildBranchingTopology(t *testing.T) {
	ga := &GraphAgent{
		Name: "article",
		Edges: func(g *Graph) []workflow.Edge {
			draft := g.Agent(&stubAgent{name: "draft"}, workflow.NodeConfig{})
			route := noopNode("route")
			polish := g.Agent(&stubAgent{name: "polish"}, workflow.NodeConfig{})
			seo := g.Agent(&stubAgent{name: "seo"}, workflow.NodeConfig{})
			gather := workflow.NewJoinNode("gather")

			eb := workflow.NewEdgeBuilder()
			eb.AddRoute(route, polish, workflow.StringRoute("approved"))
			eb.AddRoute(route, seo, workflow.StringRoute("approved"))
			eb.AddRoute(route, draft, workflow.StringRoute("rejected"))
			eb.AddFanIn(gather, polish, seo)
			eb.Add(gather, noopNode("publish"))

			return workflow.Concat(
				workflow.Chain(workflow.Start, draft, route),
				eb.Build(),
			)
		},
	}

	if _, err := ga.Build(nil); err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
}
