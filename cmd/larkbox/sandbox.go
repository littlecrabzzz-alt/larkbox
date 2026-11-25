package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const sandboxBodyLimit = 1 << 20

type sandboxCommand struct {
	name, method, path string
	body               bool
}

var sandboxCommands = []sandboxCommand{
	{"state", http.MethodGet, "state", false},
	{"metrics", http.MethodGet, "metrics", false},
	{"openapi", http.MethodGet, "openapi.json", false},
	{"seed", http.MethodPost, "seed", true},
	{"reset", http.MethodPost, "reset", false},
	{"send", http.MethodPost, "messages/send", true},
	{"card-action", http.MethodPost, "cards/action", true},
	{"validate-card", http.MethodPost, "cards/validate", true},
	{"traces", http.MethodGet, "traces", false},
	{"clear-traces", http.MethodDelete, "traces", false},
	{"clock", http.MethodGet, "clock", false},
	{"advance", http.MethodPost, "clock/advance", true},
	{"chats", http.MethodGet, "chats", false},
	{"open-chat", http.MethodPost, "chats/open", true},
	{"cards", http.MethodGet, "cards", false},
	{"sessions", http.MethodGet, "ws/sessions", false},
	{"faults", http.MethodGet, "faults", false},
	{"add-fault", http.MethodPost, "faults", true},
	{"clear-faults", http.MethodDelete, "faults", false},
	{"verify-webhook", http.MethodPost, "events/webhook/verify", true},
	{"inject-event", http.MethodPost, "events/inject", true},
	{"expire-tokens", http.MethodPost, "tokens/expire", true},
}

func runSandbox(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || slices.Contains([]string{"help", "-h", "--help"}, args[0]) {
		_, _ = fmt.Fprintln(stdout, "Usage: larkbox sandbox <command> [flags]")
		_, _ = fmt.Fprintln(stdout, "Commands:")
		for _, cmd := range sandboxCommands {
			_, _ = fmt.Fprintf(stdout, "  %-16s %s /_sandbox/%s\n", cmd.name, cmd.method, cmd.path)
		}
		_, _ = fmt.Fprintln(stdout, "Run a command with --help for its options.")
		return nil
	}
	index := slices.IndexFunc(sandboxCommands, func(cmd sandboxCommand) bool { return cmd.name == args[0] })
	if index < 0 {
		return fmt.Errorf("unknown sandbox command %q (see larkbox sandbox --help)", args[0])
	}
	cmd := sandboxCommands[index]
	flags := flag.NewFlagSet("sandbox "+cmd.name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	base := flags.String("url", "", "sandbox base URL (default LARKBOX_URL or http://127.0.0.1:8080)")
	ready := flags.String("ready-file", "", "read the bound address from a server ready file")
	timeout := flags.Duration("timeout", 30*time.Second, "request timeout")
	var inline, file, by string
	if cmd.body || cmd.name == "reset" {
		flags.StringVar(&inline, "data", "", "JSON request body")
		flags.StringVar(&file, "file", "", "JSON request body file, or - for stdin")
	}
	if cmd.name == "advance" {
		flags.StringVar(&by, "by", "", "duration to advance, such as 15s")
	}
	query := url.Values{}
	if cmd.name == "traces" {
		for _, key := range []string{"direction", "request_id", "app_id", "method", "path_prefix", "event_type", "event_id", "channel", "limit"} {
			flags.Func(strings.ReplaceAll(key, "_", "-"), "filter traces by "+key, func(value string) error {
				query.Set(key, value)
				return nil
			})
		}
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return errors.New("use flags for sandbox arguments and a positive timeout")
	}
	destination, err := sandboxURL(*base, *ready)
	if err != nil {
		return err
	}
	body, err := sandboxBody(inline, file, by, stdin)
	if err != nil {
		return err
	}
	if cmd.body && body == "" {
		return fmt.Errorf("sandbox %s requires a JSON body (see --help)", cmd.name)
	}
	destination.Path = strings.TrimRight(destination.Path, "/") + "/_sandbox/" + cmd.path
	destination.RawPath = ""
	destination.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, cmd.method, destination.String(), strings.NewReader(body))
	if err != nil {
		return err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret := os.Getenv("LARKBOX_SANDBOX_TOKEN"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	client := http.Client{Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// #nosec G107 -- The operator selects the local or shared sandbox URL.
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sandbox %s returned HTTP %d: %s", cmd.name, resp.StatusCode, strings.TrimSpace(string(message)))
	}
	_, err = io.Copy(stdout, resp.Body)
	return err
}

func sandboxURL(base, ready string) (*url.URL, error) {
	if ready != "" {
		if base != "" {
			return nil, errors.New("use either --url or --ready-file")
		}
		// #nosec G304 -- The operator selects the server ready file.
		file, err := os.Open(ready)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		addr, err := io.ReadAll(io.LimitReader(file, 1024))
		if err != nil {
			return nil, err
		}
		address := strings.TrimSpace(string(addr))
		if _, _, err := net.SplitHostPort(address); err != nil {
			return nil, errors.New("ready file must contain a bound host:port")
		}
		base = "http://" + clientAddress(address)
	}
	if base == "" {
		base = os.Getenv("LARKBOX_URL")
	}
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return nil, errors.New("sandbox URL must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	return target, nil
}

func sandboxBody(inline, file, by string, stdin io.Reader) (string, error) {
	if (inline != "" && file != "") || (by != "" && (inline != "" || file != "")) {
		return "", errors.New("use one of --data, --file, or --by")
	}
	if by != "" {
		body, err := json.Marshal(map[string]string{"by": by})
		return string(body), err
	}
	if file != "" {
		source := stdin
		if file != "-" {
			// #nosec G304 -- The operator selects the request body file.
			reader, err := os.Open(file)
			if err != nil {
				return "", err
			}
			defer func() { _ = reader.Close() }()
			source = reader
		}
		body, err := io.ReadAll(io.LimitReader(source, sandboxBodyLimit+1))
		if err != nil {
			return "", err
		}
		inline = string(body)
	}
	if len(inline) > sandboxBodyLimit {
		return "", errors.New("sandbox body exceeds 1 MiB")
	}
	return inline, nil
}
