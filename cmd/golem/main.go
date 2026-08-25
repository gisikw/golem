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
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	case protocol.Capabilities:
		b, _ := json.MarshalIndent(x, "", "  ")
		fmt.Println(string(b))
	default:
		fmt.Println(v)
	}
}
func main() {
	root := flag.NewFlagSet("golem", flag.ExitOnError)
	endpoint := root.String("service", env("GOLEM_ENDPOINT", defaultEndpoint()), "golemd URL or unix:///path")
	token := root.String("token", env("GOLEM_TOKEN", ""), "bearer token for TCP endpoints")
	root.BoolVar(&jsonOut, "json", false, "JSON output")
	root.Parse(os.Args[1:])
	args := root.Args()
	if len(args) == 0 {
		fatal(fmt.Errorf("usage: golem [--service URL] [--token TOKEN] [--json] {dispatch|capabilities|status|list|await|events|artifacts|attach|attach-hint|cancel|reap|answer|gc}"))
	}
	ctx := context.Background()
	c := client.NewWithToken(*endpoint, *token)
	switch args[0] {
	case "dispatch":
		f := flag.NewFlagSet("dispatch", flag.ExitOnError)
		h := f.String("harness", "pi", "configured harness")
		model := f.String("model", "", "model")
		cwd := f.String("cwd", "", "low-level absolute working directory escape hatch")
		project := f.String("project", "", "configured project name")
		repo := f.String("repo", "", "repository URL to clone")
		ref := f.String("ref", "", "repository ref")
		worktree := f.String("worktree", "", "durable workspace resume key")
		key := f.String("key", fmt.Sprintf("cli-%d", time.Now().UnixNano()), "idempotency key")
		f.Parse(args[1:])
		if f.NArg() == 0 {
			fatal(fmt.Errorf("dispatch requires a prompt"))
		}
		var workspace *protocol.WorkspaceSelector
		resolvedCWD := ""
		if *project != "" || *repo != "" || *worktree != "" || *ref != "" {
			workspace = &protocol.WorkspaceSelector{Project: *project, Repo: *repo, Ref: *ref, Worktree: *worktree}
			if *cwd != "" {
				fatal(errors.New("--cwd cannot be combined with workspace flags"))
			}
		} else {
			if *cwd == "" {
				*cwd = "."
			}
			var e error
			resolvedCWD, e = filepath.Abs(*cwd)
			if e != nil {
				fatal(e)
			}
		}
		j, e := c.Create(ctx, protocol.CreateJob{IdempotencyKey: *key, Harness: protocol.HarnessKind(*h), Model: *model, CWD: resolvedCWD, Workspace: workspace, Prompt: strings.Join(f.Args(), " ")})
		if e != nil {
			fatal(e)
		}
		print(j)
	case "capabilities":
		caps, e := c.Capabilities(ctx)
		if e != nil {
			fatal(e)
		}
		print(caps)
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
		id := f.Arg(0)
		j, e := c.Get(ctx, id)
		if e != nil {
			fatal(e)
		}
		if j.State.Terminal() || j.State == protocol.Blocked {
			print(j)
			return
		}
		waitCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		events, errs := c.StreamEvents(waitCtx, 0, id)
		for events != nil || errs != nil {
			select {
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				if event.Kind == "job.settled" || event.Kind == "job.state" && event.State == protocol.Blocked {
					j, e = c.Get(ctx, id)
					if e != nil {
						fatal(e)
					}
					print(j)
					return
				}
			case e, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if e != nil {
					fatal(e)
				}
			case <-waitCtx.Done():
				fatal(fmt.Errorf("await timed out after %s; job was not cancelled", *timeout))
			}
		}
		fatal(errors.New("event stream ended before job settled"))
	case "events":
		f := flag.NewFlagSet("events", flag.ExitOnError)
		since := f.Int64("since", 0, "resume after global sequence")
		job := f.String("job", "", "filter by job id")
		f.Parse(args[1:])
		if *since < 0 {
			fatal(errors.New("--since must be non-negative"))
		}
		stream, errs := c.StreamEvents(ctx, *since, *job)
		enc := json.NewEncoder(os.Stdout)
		for stream != nil || errs != nil {
			select {
			case event, ok := <-stream:
				if !ok {
					stream = nil
				} else if e := enc.Encode(event); e != nil {
					fatal(e)
				}
			case e, ok := <-errs:
				if !ok {
					errs = nil
				} else if e != nil {
					fatal(e)
				}
			}
		}
	case "artifacts":
		artifactArgs := append([]string{}, args[1:]...)
		outputPath := ""
		for i := 0; i < len(artifactArgs); i++ {
			if artifactArgs[i] == "-o" {
				if i+1 >= len(artifactArgs) || outputPath != "" {
					fatal(errors.New("artifacts -o requires one file"))
				}
				outputPath = artifactArgs[i+1]
				artifactArgs = append(artifactArgs[:i], artifactArgs[i+2:]...)
				i--
			}
		}
		if len(artifactArgs) < 1 || len(artifactArgs) > 2 || len(artifactArgs) == 1 && outputPath != "" {
			fatal(errors.New("usage: golem artifacts JOB-ID [PATH] [-o FILE]"))
		}
		if len(artifactArgs) == 1 {
			listing, e := c.Artifacts(ctx, artifactArgs[0])
			if e != nil {
				fatal(e)
			}
			if jsonOut {
				print(listing)
			} else {
				for _, artifact := range listing.Artifacts {
					fmt.Printf("%s\t%d\t%s\n", artifact.Path, artifact.Size, artifact.ModifiedAt.Format(time.RFC3339))
				}
				if listing.ArtifactsTruncated {
					fmt.Fprintln(os.Stderr, "golem: artifact listing truncated")
				}
			}
			break
		}
		response, e := c.FetchArtifact(ctx, artifactArgs[0], artifactArgs[1])
		if e != nil {
			fatal(e)
		}
		defer response.Body.Close()
		out := io.Writer(os.Stdout)
		var file *os.File
		if outputPath != "" {
			file, e = os.Create(outputPath)
			if e != nil {
				fatal(e)
			}
			defer file.Close()
			out = file
		}
		if _, e = io.Copy(out, response.Body); e != nil {
			fatal(e)
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
	case "attach":
		need(args, 2)
		j, e := c.Get(ctx, args[1])
		if e != nil {
			fatal(e)
		}
		var command string
		var argv []string
		if j.Terminal != nil {
			if info, statErr := os.Stat(j.Terminal.Socket); statErr == nil && info.Mode()&os.ModeSocket != 0 {
				command = "tmux"
				argv = []string{"tmux", "-S", j.Terminal.Socket, "attach-session", "-t", j.Terminal.Target}
			}
		}
		if command == "" && j.Activation != nil {
			command = "ssh"
			argv = []string{"ssh", "-p", fmt.Sprint(j.Activation.Port), j.Activation.User + "@" + j.Activation.Host}
		}
		if command == "" {
			fatal(errors.New("job has no live attach endpoint"))
		}
		path, e := exec.LookPath(command)
		if e != nil {
			fatal(e)
		}
		fmt.Fprintf(os.Stderr, "golem: exec %s\n", strings.Join(argv, " "))
		if e = syscall.Exec(path, argv, os.Environ()); e != nil {
			fatal(e)
		}
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
func defaultEndpoint() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "unix://golemd.sock"
	}
	return "unix://" + filepath.Join(home, ".local", "state", "golem", "golemd.sock")
}
