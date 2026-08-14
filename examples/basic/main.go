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

type echoLoop struct{}

func (echoLoop) Run(_ context.Context, input string) (string, error) {
	return input, nil
}

type namedTool string

func (t namedTool) Name() string { return string(t) }

type staticModule struct {
	id            string
	contributions []agentslot.Contribution
	requirements  []agentslot.Requirement
}

func (m staticModule) ID() string { return m.id }

func (m staticModule) Register(registrar agentslot.Registrar) error {
	return registrar.Contribute(m.contributions...)
}

func (m staticModule) RequiredSlots() []agentslot.Requirement {
	return m.requirements
}

func main() {
	builder := agentslot.NewBuilder()
	for _, module := range []agentslot.Module{
		staticModule{
			id: "basic.application",
			requirements: []agentslot.Requirement{
				agentslot.RequireOne(agentLoopSlot),
				agentslot.RequireKey(toolSlot, "shell"),
			},
		},
		staticModule{
			id: "basic.bundle",
			contributions: []agentslot.Contribution{
				agentslot.Set(agentLoopSlot, agentLoop(echoLoop{})),
				agentslot.Add(toolSlot, "shell", tool(namedTool("shell"))),
				agentslot.Add(toolSlot, "files", tool(namedTool("files"))),
			},
		},
	} {
		if err := builder.Install(module); err != nil {
			log.Fatal(err)
		}
	}

	plan, err := builder.Build(
		agentslot.RequireOne(agentLoopSlot),
		agentslot.RequireMany(toolSlot, 1),
	)
	if err != nil {
		log.Fatal(err)
	}

	runtime, err := plan.Start(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := runtime.Stop(context.Background()); err != nil {
			log.Printf("stop: %v", err)
		}
	}()

	loop, _ := agentslot.Get(plan, agentLoopSlot)
	result, err := loop.Run(context.Background(), "assembled")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s with %d tools\n", result, len(agentslot.All(plan, toolSlot)))
}
