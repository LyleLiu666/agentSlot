package main

import (
	"context"
	"fmt"
	"log"

	agentslot "github.com/LyleLiu666/agentSlot"
)

type agentLoop interface {
	Run(context.Context, string) (string, error)
}

type tool interface {
	Name() string
}

var (
	agentLoopSlot = agentslot.One[agentLoop]("agent.loop")
	toolSlot      = agentslot.Many[tool]("tool")
)

type echoLoop struct {
	tools []tool
}

func (l echoLoop) Run(_ context.Context, input string) (string, error) {
	return fmt.Sprintf("%s with %d automatically mounted tools", input, len(l.tools)), nil
}

type namedTool string

func (t namedTool) Name() string { return string(t) }

type toolModule struct {
	key  string
	tool tool
}

func (m toolModule) ID() string { return "basic.tool." + m.key }

func (m toolModule) Register(registrar agentslot.Registrar) error {
	return registrar.Contribute(agentslot.Add(toolSlot, m.key, m.tool))
}

type loopModule struct{}

func (loopModule) ID() string { return "basic.loop" }

func (loopModule) Register(registrar agentslot.Registrar) error {
	return registrar.Contribute(agentslot.SetWith(agentLoopSlot, func(resolver agentslot.Resolver) (agentLoop, error) {
		registered, err := agentslot.ResolveMany(resolver, toolSlot)
		if err != nil {
			return nil, err
		}
		tools := make([]tool, 0, len(registered))
		for _, named := range registered {
			tools = append(tools, named.Value)
		}
		return echoLoop{tools: tools}, nil
	}))
}

func (loopModule) RequiredSlots() []agentslot.Requirement {
	return []agentslot.Requirement{agentslot.RequireMany(toolSlot, 1)}
}

func main() {
	application := agentslot.NewApplication(
		"basic-agent",
		[]agentslot.Module{
			loopModule{},
			toolModule{key: "shell", tool: namedTool("shell")},
			toolModule{key: "files", tool: namedTool("files")},
		},
		agentslot.RequireOne(agentLoopSlot),
		agentslot.RequireMany(toolSlot, 1),
	)

	plan, err := application.Build()
	if err != nil {
		log.Fatal(err)
	}

	runtime, err := application.Start(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := runtime.Stop(context.Background()); err != nil {
			log.Printf("stop: %v", err)
		}
	}()

	if runtime.Plan() != plan {
		log.Fatal("runtime did not start the built plan")
	}
	loop, _ := agentslot.Get(plan, agentLoopSlot)
	result, err := loop.Run(context.Background(), "assembled")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result)
}
