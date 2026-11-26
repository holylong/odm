// Command odm is a functional reimplementation of Internet Download Manager's
// command-line surface.
//
// Observed CLI shape (re/FINDINGS.md §9, from the shipped command line help):
//
//	idman /d URL [/p local_path] [/f local_file_name] [/q] [/h] [/n] [/a]
//	idman /s
//
// ODM accepts the same switches with a leading slash on Windows and a leading
// dash elsewhere, plus its own long options for the service and native host.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"odm/internal/api"
	"odm/internal/config"
	"odm/internal/engine"
	"odm/internal/host"
)

const version = "1.0.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "odm:", err)
		os.Exit(1)
	}
}

func run() error {
	args, opts := splitArgs(os.Args[1:])

	// Own long options take precedence over the IDM switch set.
	fs := flag.NewFlagSet("odm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	port := fs.Int("port", 0, "offline bridge port (default from settings)")
	serve := fs.Bool("serve", false, "run only the local service, no download")
	noBrowser := fs.Bool("no-browser", false, "do not open the UI in a browser")
	nativeHost := fs.Bool("native-host", false, "run as a browser native-messaging host on stdio")
	showVersion := fs.Bool("version", false, "print the version and exit")
	printConfig := fs.Bool("config", false, "print the settings file path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case *showVersion:
		fmt.Printf("ODM %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return nil
	case *printConfig:
		fmt.Println(config.Path())
		return nil
	case *nativeHost:
		return runNativeHost(*port)
	}

	set := config.Current()
	if *port > 0 {
		set.BridgePort = *port
	}

	mgr := engine.NewManager(set.TempDir, func(f string, a ...any) {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
	})

	// /s: start the scheduler queue and leave.
	if opts.StartQueue {
		n := mgr.ResumePending()
		fmt.Printf("odm: started %d queued download(s)\n", n)
		if n == 0 {
			return nil
		}
	}

	// /d: add a download from the command line.
	var created *engine.Task
	if opts.URL != "" {
		t, err := mgr.Add(engine.AddOptions{
			URL:      opts.URL,
			Dir:      opts.Path,
			Filename: opts.Filename,
			Referer:  opts.Referer,
			Category: opts.Category,
			QueueID:  "main",
			StartNow: !opts.QueueOnly,
		})
		if err != nil {
			return err
		}
		created = t
		fmt.Printf("odm: queued %s -> %s\n", t.URL, filepath.Join(t.Dir, t.Filename))
	}

	// The service is what the UI and the extension talk to, so it always runs
	// unless the caller only wanted a one-shot add plus keep-alive mode.
	srv := api.New(mgr, set.BridgePort, func(f string, a ...any) {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(f, a...))
	})
	if err := srv.Listen(); err != nil {
		return fmt.Errorf("cannot start the local service: %w", err)
	}
	srv.LogfRecord("ODM %s listening on %s", version, srv.Addr())
	fmt.Printf("odm: UI at %s\n", srv.Addr())

	if !opts.Hidden && !*noBrowser && !opts.QueueOnly {
		openBrowser(srv.Addr())
	}

	if opts.Quit || created == nil {
		if opts.Quit && created != nil {
			waitForIdle(mgr, created.ID, 0)
		}
		if opts.Quit {
			_ = srv.Close()
			return nil
		}
	}

	// Keep serving until interrupted.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	if *serve || created == nil {
		<-stop
	} else {
		select {
		case <-stop:
		case <-waitDone(mgr, created.ID):
		}
	}
	_ = srv.Close()
	fmt.Println("odm: stopped")
	return nil
}

// waitDone returns a channel closed when the task leaves the active states.
func waitDone(mgr *engine.Manager, id string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for {
			t := mgr.Get(id)
			if t == nil || t.Status == engine.StatusComplete || t.Status == engine.StatusError {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()
	return ch
}

func waitForIdle(mgr *engine.Manager, id string, _ int) {
	<-waitDone(mgr, id)
}

// ------------------------------------------------------------------ options

type cliOptions struct {
	URL        string
	Path       string
	Filename   string
	Referer    string
	Category   string
	Quit       bool
	Hidden     bool
	Silent     bool
	QueueOnly  bool
	StartQueue bool
}

// splitArgs extracts IDM-style /x switches and leaves the rest for flag.Parse.
// Both "/d" and "-d" are accepted so the same invocation works from cmd.exe and
// from a POSIX shell.
func splitArgs(argv []string) ([]string, cliOptions) {
	var rest []string
	var o cliOptions
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if len(a) < 2 || (a[0] != '/' && a[0] != '-') {
			rest = append(rest, a)
			continue
		}
		sw := strings.ToLower(a[1:])
		next := func() string {
			if i+1 < len(argv) {
				i++
				return argv[i]
			}
			return ""
		}
		switch sw {
		case "d":
			o.URL = next()
		case "p":
			o.Path = next()
		case "f":
			o.Filename = next()
		case "c":
			o.Category = next()
		case "r":
			o.Referer = next()
		case "q":
			o.Quit = true
		case "h":
			o.Hidden = true
		case "n":
			o.Silent = true
		case "a":
			o.QueueOnly = true
		case "s":
			o.StartQueue = true
		default:
			// Not an IDM switch: hand it to the Go flag set.
			rest = append(rest, a)
		}
	}
	return rest, o
}

// ------------------------------------------------------------- native host

// runNativeHost proxies extension requests to the running ODM service over the
// loopback HTTP API. The service owns the task list, so the host process stays
// stateless and can be started and killed by the browser at will.
func runNativeHost(port int) error {
	base := fmt.Sprintf("http://127.0.0.1:%d", resolvePort(port))
	client := &http.Client{Timeout: 20 * time.Second}

	return host.Stdio(func(m *host.Message) *host.Reply {
		switch m.Type {
		case "ping":
			return &host.Reply{OK: true, App: "odm", Version: version, Port: resolvePort(port)}
		case "takeover":
			body := map[string]any{
				"url": m.URL, "referer": m.Referer, "filename": m.Filename,
				"mime": m.Mime, "size": m.Size, "userAgent": m.UserAgent,
				"tabID": m.TabID, "silent": m.Silent,
			}
			var out struct {
				Takeover bool   `json:"takeover"`
				Reason   string `json:"reason"`
				TaskID   string `json:"taskID"`
				Error    string `json:"error"`
			}
			if err := postJSON(client, base+"/api/bridge/takeover", body, &out); err != nil {
				return &host.Reply{OK: false, Error: err.Error()}
			}
			return &host.Reply{OK: out.Error == "", Takeover: out.Takeover, Reason: out.Reason,
				TaskID: out.TaskID, Error: out.Error, Port: resolvePort(port)}
		case "settings":
			var out map[string]any
			if err := getJSON(client, base+"/api/settings", &out); err != nil {
				return &host.Reply{OK: false, Error: err.Error()}
			}
			b, _ := json.Marshal(out)
			return &host.Reply{OK: true, Reason: string(b)}
		default:
			return &host.Reply{OK: false, Error: "unknown message type " + m.Type}
		}
	})
}

// resolvePort returns the configured bridge port, probing upward for a live one.
func resolvePort(port int) int {
	if port > 0 {
		return port
	}
	p := config.Current().BridgePort
	for i := 0; i < 12; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/bridge/ping", p))
		if err == nil {
			resp.Body.Close()
			return p
		}
		p++
	}
	return config.Current().BridgePort
}

func postJSON(c *http.Client, url string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := c.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func getJSON(c *http.Client, url string, out any) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// ------------------------------------------------------------------- browser

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
