package commands

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/concourse/atc"
	"github.com/concourse/fly/commands/internal/executehelpers"
	"github.com/concourse/fly/commands/internal/flaghelpers"
	"github.com/concourse/fly/config"
	"github.com/concourse/fly/eventstream"
	"github.com/concourse/fly/rc"
	"github.com/concourse/go-concourse/concourse"
)

type ExecuteCommand struct {
	TaskConfig     flaghelpers.PathFlag         `short:"c" long:"config" required:"true"                description:"The task config to execute"`
	Privileged     bool                         `short:"p" long:"privileged"                            description:"Run the task with full privileges"`
	ExcludeIgnored bool                         `short:"x" long:"exclude-ignored"                       description:"Skip uploading .gitignored paths. This uses the file paths that are in your Git index. Make sure it's up to date!"`
	Inputs         []flaghelpers.InputPairFlag  `short:"i" long:"input"       value-name:"NAME=PATH"    description:"An input to provide to the task (can be specified multiple times)"`
	InputsFrom     flaghelpers.JobFlag          `short:"j" long:"inputs-from" value-name:"PIPELINE/JOB" description:"A job to base the inputs on"`
	Outputs        []flaghelpers.OutputPairFlag `short:"o" long:"output"      value-name:"NAME=PATH"    description:"An output to fetch from the task (can be specified multiple times)"`
	Tags           []string                     `          long:"tag"         value-name:"TAG"          description:"A tag for a specific environment (can be specified multiple times)"`
}

func (command *ExecuteCommand) Execute(args []string) error {
	client, err := rc.TargetClient(Fly.Target)
	if err != nil {
		return err
	}
	err = rc.ValidateClient(client, Fly.Target, false)
	if err != nil {
		return err
	}

	taskConfigFile := command.TaskConfig
	excludeIgnored := command.ExcludeIgnored

	taskConfig, err := config.LoadTaskConfig(string(taskConfigFile), args)
	if err != nil {
		return err
	}

	inputs, err := executehelpers.DetermineInputs(
		client,
		taskConfig.Inputs,
		command.Inputs,
		command.InputsFrom,
	)
	if err != nil {
		return err
	}

	outputs, err := executehelpers.DetermineOutputs(
		client,
		taskConfig.Outputs,
		command.Outputs,
	)
	if err != nil {
		return err
	}

	build, err := executehelpers.CreateBuild(
		client,
		command.Privileged,
		inputs,
		outputs,
		taskConfig,
		command.Tags,
		Fly.Target,
	)
	if err != nil {
		return err
	}

	fmt.Println("executing build", build.ID)

	terminate := make(chan os.Signal, 1)

	go abortOnSignal(client, terminate, build)

	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

	inputChan := make(chan interface{})
	go func() {
		for _, i := range inputs {
			if i.Path != "" {
				executehelpers.Upload(client, i, excludeIgnored)
			}
		}
		close(inputChan)
	}()

	var outputChans []chan (interface{})
	if len(outputs) > 0 {
		for i, output := range outputs {
			outputChans = append(outputChans, make(chan interface{}, 1))
			go func(o executehelpers.Output, outputChan chan<- interface{}) {
				if o.Path != "" {
					executehelpers.Download(client, o)
				}

				close(outputChan)
			}(output, outputChans[i])
		}
	}

	eventSource, err := client.BuildEvents(fmt.Sprintf("%d", build.ID))
	if err != nil {
		return err
	}

	exitCode := eventstream.Render(os.Stdout, eventSource)
	eventSource.Close()

	<-inputChan

	if len(outputs) > 0 {
		for _, outputChan := range outputChans {
			<-outputChan
		}
	}

	os.Exit(exitCode)

	return nil
}

func abortOnSignal(
	client concourse.Client,
	terminate <-chan os.Signal,
	build atc.Build,
) {
	<-terminate

	fmt.Fprintf(os.Stderr, "\naborting...\n")

	err := client.AbortBuild(strconv.Itoa(build.ID))
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to abort:", err)
		return
	}

	// if told to terminate again, exit immediately
	<-terminate
	fmt.Fprintln(os.Stderr, "exiting immediately")
	os.Exit(2)
}
