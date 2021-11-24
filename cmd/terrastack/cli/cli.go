package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/mineiros-io/terrastack"
)

type cliSpec struct {
	Version struct{} `cmd:"" help:"Terrastack version."`

	GitChangeBase string `short:"B" optional:"true" default:"main" help:"git base ref for computing changes."`

	Init struct {
		StackDirs []string `arg:"" name:"paths" optional:"true" help:"the stack directory (current directory if not set)."`
		Force     bool     `help:"force initialization."`
	} `cmd:"" help:"Initialize a stack."`

	List struct {
		Changed bool   `short:"c" help:"Shows only changed stacks."`
		Why     bool   `help:"Shows reason on why the stack has changed."`
		BaseDir string `arg:"" optional:"true" name:"path" type:"path" help:"base stack directory."`
	} `cmd:"" help:"List stacks."`

	Run struct {
		Quiet   bool     `short:"q" help:"Don't print any information other than the command output."`
		Changed bool     `short:"c" help:"Run on all changed stacks."`
		Basedir string   `short:"b" optional:"true" help:"Run on stacks inside basedir."`
		Command []string `arg:"" name:"cmd" passthrough:"" help:"command to execute."`
	} `cmd:"" help:"Run command in the stacks."`
}

// Run will run terrastack with the provided flags defined on args.
// Only flags should be on the args slice.

// Results will be written on stdout, according to the
// command flags. Any partial/non-critical errors will be
// written on stderr.
//
// Sometimes sub commands may be executed, the provided stdin
// will be passed to then as the sub process stdin.
//
// Each Run call is completely isolated from each other (no shared state)
// as far as the parameters are not shared between the Run calls.
//
// If a critical error is found an non-nil error is returned.
func Run(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	c, err := newCLI(args, stdin, stdout, stderr)
	if err != nil {
		return err
	}
	return c.run()
}

type cli struct {
	ctx        *kong.Context
	parsedArgs *cliSpec
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
}

func newCLI(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) (*cli, error) {
	parsedArgs := cliSpec{}
	parser, err := kong.New(&parsedArgs,
		kong.Name("terrastack"),
		kong.Description("A tool for managing terraform stacks"),
		kong.UsageOnError(),
		kong.ConfigureHelp(kong.HelpOptions{
			Compact: true,
		}),
		kong.Writers(stdout, stderr))
	if err != nil {
		return nil, fmt.Errorf("failed to create cli parser: %v", err)
	}

	ctx, err := parser.Parse(args)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cli args %v: %v", args, err)
	}

	return &cli{
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		parsedArgs: &parsedArgs,
		ctx:        ctx,
	}, nil
}

func (c *cli) run() error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cli.run(): failed to get process working dir: %v", err)
	}
	switch c.ctx.Command() {
	case "version":
		c.log(terrastack.Version())
	case "init":
		return c.initStack(wd, []string{wd})
	case "init <paths>":
		return c.initStack(wd, c.parsedArgs.Init.StackDirs)
	case "list":
		return c.printStacks(wd, wd)
	case "list <path>":
		return c.printStacks(c.parsedArgs.List.BaseDir, wd)
	case "run":
		if len(c.parsedArgs.Run.Command) == 0 {
			return errors.New("no command specified")
		}
		fallthrough
	case "run <cmd>":
		basedir := wd
		if c.parsedArgs.Run.Basedir != "" {
			basedir = strings.TrimSuffix(c.parsedArgs.Run.Basedir, "/")
		}
		return c.runOnStacks(basedir)

	default:
		return fmt.Errorf("unexpected command sequence: %s", c.ctx.Command())
	}

	return nil
}

func (c *cli) initStack(basedir string, dirs []string) error {
	var nErrors int
	mgr := terrastack.NewManager(basedir, c.parsedArgs.GitChangeBase)
	for _, d := range dirs {
		err := mgr.Init(d, c.parsedArgs.Init.Force)
		if err != nil {
			c.logerr("warn: failed to initialize stack: %v", err)
			nErrors++
		}
	}

	if nErrors > 0 {
		return fmt.Errorf("failed to initialize %d stack(s)", nErrors)
	}
	return nil
}

func (c *cli) listStacks(mgr *terrastack.Manager, isChanged bool) ([]terrastack.Entry, error) {
	var (
		err    error
		stacks []terrastack.Entry
	)

	if isChanged {
		stacks, err = mgr.ListChanged()
	} else {
		stacks, err = mgr.List()
	}

	return stacks, err
}

func (c *cli) printStacks(basedir string, cwd string) error {
	mgr := terrastack.NewManager(basedir, c.parsedArgs.GitChangeBase)
	stacks, err := c.listStacks(mgr, c.parsedArgs.List.Changed)
	if err != nil {
		return fmt.Errorf("can't list stacks: %v", err)
	}

	cwd = cwd + string(os.PathSeparator)

	for _, stack := range stacks {
		stackdir := strings.TrimPrefix(stack.Dir, cwd)

		if c.parsedArgs.List.Why {
			c.log("%s - %s", stackdir, stack.Reason)
		} else {
			c.log(stackdir)
		}
	}
	return nil
}

func (c *cli) runOnStacks(basedir string) error {
	var nErrors int

	basedir, err := filepath.Abs(basedir)
	if err != nil {
		return fmt.Errorf("can't find absolute path for %q: %v", basedir, err)
	}

	mgr := terrastack.NewManager(basedir, c.parsedArgs.GitChangeBase)
	stacks, err := c.listStacks(mgr, c.parsedArgs.Run.Changed)
	if err != nil {
		return fmt.Errorf("can't list stacks: %v", err)
	}

	if c.parsedArgs.Run.Changed {
		c.log("Running on changed stacks:")
	} else {
		c.log("Running on all stacks:")
	}

	cmdName := c.parsedArgs.Run.Command[0]
	args := c.parsedArgs.Run.Command[1:]

	for _, stack := range stacks {

		cmd := exec.Command(cmdName, args...)
		cmd.Dir = stack.Dir
		cmd.Stdin = c.stdin
		cmd.Stdout = c.stdout
		cmd.Stderr = c.stderr

		c.log("[%s] running %s", stack.Dir, cmd)

		err = cmd.Run()
		if err != nil {
			c.logerr("warn: failed to execute command: %v", err)
			nErrors++
		}
	}

	if nErrors != 0 {
		return fmt.Errorf("some (%d) commands failed", nErrors)
	}

	return nil
}

func (c *cli) log(format string, args ...interface{}) {
	fmt.Fprintln(c.stdout, fmt.Sprintf(format, args...))
}

func (c *cli) logerr(format string, args ...interface{}) {
	fmt.Fprintln(c.stderr, fmt.Sprintf(format, args...))
}