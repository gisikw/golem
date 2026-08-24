package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gisikw/golem/client"
	"github.com/gisikw/golem/protocol"
	"github.com/gisikw/golem/supervisor"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var jsonOut bool

func fatal(e error) { fmt.Fprintln(os.Stderr, "golem:", e); os.Exit(1) }
func print(v any) {
	if jsonOut {
		json.NewEncoder(os.Stdout).Encode(v)
		return
	}
	switch x := v.(type) {
	case protocol.Job:
		fmt.Printf("%s  %-11s %-8s host=%s\n", x.ID, x.State, x.Harness, x.Host)
	case []protocol.Job:
		for _, j := range x {
			print(j)
		}
	default:
		fmt.Println(v)
	}
}
func main() {
	root := flag.NewFlagSet("golem", flag.ExitOnError)
	endpoint := root.String("service", env("GOLEM_ENDPOINT", "http://127.0.0.1:7337"), "service URL or unix:///path")
	root.BoolVar(&jsonOut, "json", false, "JSON output")
	root.Parse(os.Args[1:])
	args := root.Args()
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: golem [--service URL] [--json] {dispatch|status|list|await|attach-hint|cancel|reap|answer|gc}"))
	}
	ctx := context.Background()
	c := client.New(*endpoint)
	switch args[0] {
	case "dispatch":
		f := flag.NewFlagSet("dispatch", flag.ExitOnError)
		host := f.String("host", "", "worker host")
		h := f.String("harness", "pi", "pi|claude|codex|fake")
		model := f.String("model", "", "model")
		cwd := f.String("cwd", ".", "working directory")
		key := f.String("key", fmt.Sprintf("cli-%d", time.Now().UnixNano()), "idempotency key")
		worktree := f.Bool("worktree", false, "use detached git worktree")
		// provider-config is an opaque single-provider connection descriptor the
		// dispatching client resolves at dispatch time so the worker
		// boots with exactly the dispatched provider+model. See protocol.ProviderConfig.
		providerConfig := f.String("provider-config", "", "JSON protocol.ProviderConfig (worker single-provider descriptor)")
		f.Parse(args[1:])
		if *host == "" || f.NArg() == 0 {
			fatal(fmt.Errorf("dispatch requires --host and prompt"))
		}
		abs, e := filepath.Abs(*cwd)
		if e != nil {
			fatal(e)
		}
		iso := protocol.IsolationNone
		if *worktree {
			iso = protocol.IsolationWorktree
		}
		var pc *protocol.ProviderConfig
		if *providerConfig != "" {
			pc = &protocol.ProviderConfig{}
			if err := json.Unmarshal([]byte(*providerConfig), pc); err != nil || protocol.ValidateProviderConfig(pc) != nil {
				// Do not echo the supplied JSON: it may contain a credential.
				fatal(errors.New("invalid --provider-config"))
			}
		}
		j, e := c.Create(ctx, protocol.CreateJob{IdempotencyKey: *key, Harness: protocol.HarnessKind(*h), Model: *model, ProviderConfig: pc, CWD: abs, Isolation: iso, Prompt: strings.Join(f.Args(), " "), Host: *host})
		if e != nil {
			fatal(e)
		}
		print(j)
	case "status":
		need(args, 2)
		j, e := c.Get(ctx, args[1])
		if e != nil {
			fatal(e)
		}
		print(j)
	case "list":
		f := flag.NewFlagSet("list", flag.ExitOnError)
		state := f.String("state", "", "lifecycle state")
		f.Parse(args[1:])
		j, e := c.List(ctx, protocol.State(*state))
		if e != nil {
			fatal(e)
		}
		print(j)
	case "await":
		f := flag.NewFlagSet("await", flag.ExitOnError)
		timeout := f.Duration("timeout", 10*time.Minute, "maximum wait; does not cancel the job")
		f.Parse(args[1:])
		if f.NArg() != 1 {
			fatal(fmt.Errorf("await requires one job id"))
		}
		deadline := time.Now().Add(*timeout)
		for {
			j, e := c.Get(ctx, f.Arg(0))
			if e != nil {
				fatal(e)
			}
			if j.State.Terminal() || j.State == protocol.Blocked {
				print(j)
				return
			}
			if time.Now().After(deadline) {
				fatal(fmt.Errorf("await timed out after %s; job was not cancelled", *timeout))
			}
			time.Sleep(500 * time.Millisecond)
		}
	case "cancel":
		need(args, 2)
		j, e := c.Cancel(ctx, args[1])
		if e != nil {
			fatal(e)
		}
		print(j)
	case "reap":
		need(args, 2)
		j, e := c.Reap(ctx, args[1])
		if e != nil {
			fatal(e)
		}
		print(j)
	case "answer":
		need(args, 3)
		j, e := c.Get(ctx, args[1])
		if e != nil {
			fatal(e)
		}
		qid := ""
		if j.Question != nil {
			qid = j.Question.ID
		}
		j, e = c.Answer(ctx, args[1], protocol.Answer{IdempotencyKey: fmt.Sprintf("cli-%d", time.Now().UnixNano()), QuestionID: qid, Text: strings.Join(args[2:], " ")})
		if e != nil {
			fatal(e)
		}
		print(j)
	case "attach-hint":
		need(args, 2)
		j, e := c.Get(ctx, args[1])
		if e != nil {
			fatal(e)
		}
		if j.Terminal == nil {
			fatal(fmt.Errorf("terminal endpoint not yet published"))
		}
		if jsonOut {
			print(j.Terminal)
		} else {
			fmt.Printf("host=%s tmux -S %q attach-session -t %q\n", j.Terminal.Host, j.Terminal.Socket, j.Terminal.Target)
		}
	case "gc":
		f := flag.NewFlagSet("gc", flag.ExitOnError)
		root := f.String("root", "artifacts", "artifact root")
		age := f.Duration("older-than", 30*24*time.Hour, "minimum age")
		f.Parse(args[1:])
		jobs, e := c.List(ctx, "")
		if e != nil {
			fatal(e)
		}
		if e = supervisor.GCSettled(jobs, *root, time.Now(), *age); e != nil {
			fatal(e)
		}
	default:
		fatal(fmt.Errorf("unknown command %q", args[0]))
	}
}
func need(a []string, n int) {
	if len(a) < n {
		fatal(fmt.Errorf("missing argument"))
	}
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
