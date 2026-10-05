//
// BEWARE CODE BELOW IS COMPLETELY AI GENERATED.
// I COULD NOT BE BOTHERED WRITING THIS MY SELF.
//

// lp-sync declaratively syncs LuckPerms groups ("roles") and their permissions
// from a YAML file via the LuckPerms REST API, terraform-style.
//
// Usage:
//
//	lp-sync plan  <config.yaml>                     show what would change
//	lp-sync apply <config.yaml> [-y|--auto-approve] apply the changes
//
// Config format:
//
//	roles:
//	  - name: example-role
//	    permissions:
//	      - spacechunks.example-plugin.some-permission
//
// Environment:
//
//	LP_API_URL           Base URL of the REST API, e.g. http://localhost:8080 (required)
//	LP_TOKEN_URL         OAuth2 token endpoint                                (required)
//	LP_CLIENT_ID         OAuth2 client id                                     (required)
//	LP_CLIENT_SECRET     OAuth2 client secret                                 (required)
//	LP_SCOPE             Space-separated scopes to request                    (optional)
//	LP_OAUTH_AUTH_STYLE  How to send client credentials: "basic" or "body"    (default: auto-detect)
//	LP_PROTECTED_GROUPS  Space-separated groups that are never deleted        (default: "default")
//
// Authentication: an access token is fetched with the OAuth2 client credentials
// grant and sent as "Authorization: Bearer <token>". It is refreshed shortly
// before it expires, and once on a 401 from the API.
//
// What is managed:
//   - Groups: every group not in the config is deleted (except protected ones).
//   - Permissions: only global permission nodes (type "permission", no context,
//     no expiry). Inheritance, prefix/suffix, meta, weight, contextual and
//     temporary nodes are left untouched.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const usage = `Usage:
  lp-sync plan  <config.yaml>                     show what would change
  lp-sync apply <config.yaml> [-y|--auto-approve] apply the changes

Environment:
  LP_API_URL           Base URL of the REST API (required)
  LP_TOKEN_URL         OAuth2 token endpoint (required)
  LP_CLIENT_ID         OAuth2 client id (required)
  LP_CLIENT_SECRET     OAuth2 client secret (required)
  LP_SCOPE             Space-separated scopes to request
  LP_OAUTH_AUTH_STYLE  "basic" or "body" (default: auto-detect)
  LP_PROTECTED_GROUPS  Space-separated groups never deleted (default: "default")
`

var roleNameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ------------------------------------------------------------------ config --

type Config struct {
	Roles *[]Role `yaml:"roles"`
}

type Role struct {
	Name        string   `yaml:"name"`
	Permissions []string `yaml:"permissions"`
}

func loadConfig(path string) ([]Role, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f, yaml.DisallowUnknownField()) // catch typos like "permision"
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		// FormatError prints the offending line of the file with a pointer to it.
		return nil, fmt.Errorf("could not parse %s:\n%s", path, yaml.FormatError(err, isTerminal(os.Stderr), true))
	}
	if cfg.Roles == nil {
		return nil, errors.New("config must contain a 'roles' list (use 'roles: []' to remove everything)")
	}

	roles := *cfg.Roles
	seen := map[string]bool{}
	var problems []string
	for i := range roles {
		r := &roles[i]
		switch {
		case r.Name == "":
			problems = append(problems, fmt.Sprintf("role #%d has no name", i+1))
		case !roleNameRe.MatchString(r.Name):
			problems = append(problems, fmt.Sprintf("invalid role name %q (use lowercase a-z, 0-9, _ and -)", r.Name))
		case seen[r.Name]:
			problems = append(problems, fmt.Sprintf("duplicate role name %q", r.Name))
		}
		seen[r.Name] = true

		for _, p := range r.Permissions {
			if strings.TrimSpace(p) == "" {
				problems = append(problems, fmt.Sprintf("role %q has an empty permission", r.Name))
			}
		}
		slices.Sort(r.Permissions)
		r.Permissions = slices.Compact(r.Permissions)
	}
	if len(problems) > 0 {
		return nil, errors.New("invalid config:\n  " + strings.Join(problems, "\n  "))
	}
	return roles, nil
}

// -------------------------------------------------------------- api client --

type Context struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Node struct {
	Key     string    `json:"key"`
	Type    string    `json:"type,omitempty"`
	Value   bool      `json:"value"`
	Context []Context `json:"context"`
	Expiry  *int64    `json:"expiry,omitempty"`
}

// NewNode is the request shape for adding / deleting nodes.
type NewNode struct {
	Key     string    `json:"key"`
	Value   bool      `json:"value"`
	Context []Context `json:"context"`
}

type APIError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *APIError) Error() string {
	return strings.TrimSpace(fmt.Sprintf("%s %s -> HTTP %d %s", e.Method, e.Path, e.Status, e.Body))
}

// Client talks to the LuckPerms REST API. Its HTTP client comes from
// golang.org/x/oauth2/clientcredentials, which fetches the access token,
// adds it as "Authorization: Bearer <token>" and refreshes it before expiry.
type Client struct {
	base  string
	ctx   context.Context
	oauth *clientcredentials.Config
	http  *http.Client
}

func NewClient(ctx context.Context, base string, oauth *clientcredentials.Config) *Client {
	c := &Client{base: base, ctx: ctx, oauth: oauth}
	c.resetToken()
	return c
}

// resetToken builds a fresh oauth2 HTTP client, discarding the cached token.
func (c *Client) resetToken() {
	c.http = c.oauth.Client(c.ctx)
	c.http.Timeout = 30 * time.Second
}

func (c *Client) do(method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}

	resp, err := c.send(method, path, payload)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		// Token may have been revoked or expired early: get a new one and retry once.
		resp.Body.Close()
		c.resetToken()
		resp, err = c.send(method, path, payload)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{method, path, resp.StatusCode, string(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) send(method, path string, payload []byte) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var rerr *oauth2.RetrieveError
		if errors.As(err, &rerr) {
			msg := strings.TrimSpace(rerr.ErrorCode + ": " + rerr.ErrorDescription)
			if rerr.ErrorCode == "" {
				msg = strings.TrimSpace(string(rerr.Body))
			}
			return nil, fmt.Errorf("fetching access token: HTTP %d %s", rerr.Response.StatusCode, msg)
		}
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

func groupPath(name string) string { return "/group/" + url.PathEscape(name) }

func (c *Client) Groups() (names []string, err error) {
	return names, c.do(http.MethodGet, "/group", nil, &names)
}
func (c *Client) GroupNodes(g string) (nodes []Node, err error) {
	return nodes, c.do(http.MethodGet, groupPath(g)+"/nodes", nil, &nodes)
}
func (c *Client) CreateGroup(g string) error {
	return c.do(http.MethodPost, "/group", map[string]string{"name": g}, nil)
}
func (c *Client) DeleteGroup(g string) error {
	return c.do(http.MethodDelete, groupPath(g), nil, nil)
}
func (c *Client) AddNodes(g string, nodes []NewNode) error {
	return c.do(http.MethodPatch, groupPath(g)+"/nodes", nodes, nil)
}
func (c *Client) RemoveNodes(g string, nodes []NewNode) error {
	// An empty body would delete ALL nodes of the group - never send one.
	if len(nodes) == 0 {
		return nil
	}
	return c.do(http.MethodDelete, groupPath(g)+"/nodes", nodes, nil)
}
func (c *Client) PushUpdate() error {
	return c.do(http.MethodPost, "/messaging/update", nil, nil)
}

// -------------------------------------------------------------------- plan --

type Action string

const (
	Create Action = "create"
	Update Action = "update"
	Delete Action = "delete"
)

type Op struct {
	Action Action
	Group  string
	Add    []string
	Remove []NewNode
}

// isManaged reports whether a node is one this tool owns.
func isManaged(n Node) bool {
	return n.Type == "permission" && len(n.Context) == 0 && n.Expiry == nil
}

func buildPlan(c *Client, roles []Role, protected []string) ([]Op, error) {
	current, err := c.Groups()
	if err != nil {
		return nil, err
	}

	var plan []Op
	desired := map[string]bool{}
	for _, r := range roles {
		desired[r.Name] = true

		if !slices.Contains(current, r.Name) {
			plan = append(plan, Op{Action: Create, Group: r.Name, Add: r.Permissions})
			continue
		}

		nodes, err := c.GroupNodes(r.Name)
		if err != nil {
			return nil, err
		}
		have := map[string]bool{} // permissions currently granted (value=true)
		var remove []NewNode
		for _, n := range nodes {
			if !isManaged(n) {
				continue
			}
			if n.Value && slices.Contains(r.Permissions, n.Key) {
				have[n.Key] = true
				continue
			}
			remove = append(remove, NewNode{Key: n.Key, Value: n.Value, Context: []Context{}})
		}
		var add []string
		for _, p := range r.Permissions {
			if !have[p] {
				add = append(add, p)
			}
		}
		if len(add) > 0 || len(remove) > 0 {
			plan = append(plan, Op{Action: Update, Group: r.Name, Add: add, Remove: remove})
		}
	}

	for _, g := range current {
		if !desired[g] && !slices.Contains(protected, g) {
			plan = append(plan, Op{Action: Delete, Group: g})
		}
	}
	return plan, nil
}

// ------------------------------------------------------------------ output --

var green, red, yellow, bold, reset string

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func printPlan(plan []Op) {
	counts := map[Action]int{}
	fmt.Println()
	for _, op := range plan {
		counts[op.Action]++
		switch op.Action {
		case Create:
			fmt.Printf("%s+ group %s%s\n", green, op.Group, reset)
		case Delete:
			fmt.Printf("%s- group %s%s\n", red, op.Group, reset)
		case Update:
			fmt.Printf("%s~ group %s%s\n", yellow, op.Group, reset)
		}
		for _, n := range op.Remove {
			suffix := ""
			if !n.Value {
				suffix = " (=false)"
			}
			fmt.Printf("%s    - %s%s%s\n", red, n.Key, suffix, reset)
		}
		for _, p := range op.Add {
			fmt.Printf("%s    + %s%s\n", green, p, reset)
		}
	}
	fmt.Printf("\n%sPlan: %d to create, %d to update, %d to delete.%s\n",
		bold, counts[Create], counts[Update], counts[Delete], reset)
}

func apply(c *Client, plan []Op) error {
	for _, op := range plan {
		switch op.Action {
		case Create:
			if err := c.CreateGroup(op.Group); err != nil {
				return err
			}
		case Delete:
			if err := c.DeleteGroup(op.Group); err != nil {
				return err
			}
		}
		if err := c.RemoveNodes(op.Group, op.Remove); err != nil {
			return err
		}
		if len(op.Add) > 0 {
			nodes := make([]NewNode, len(op.Add))
			for i, p := range op.Add {
				nodes[i] = NewNode{Key: p, Value: true, Context: []Context{}}
			}
			if err := c.AddNodes(op.Group, nodes); err != nil {
				return err
			}
		}
		fmt.Printf("%sd group %s\n", op.Action, op.Group)
	}

	// Tell other servers on the network to reload (ignored if messaging isn't set up).
	if err := c.PushUpdate(); err == nil {
		fmt.Println("Pushed update via messaging service.")
	}
	return nil
}

// -------------------------------------------------------------------- main --

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%sError:%s %s\n", red, reset, fmt.Sprintf(format, a...))
	os.Exit(1)
}

func main() {
	if isTerminal(os.Stdout) {
		green, red, yellow, bold, reset = "\033[32m", "\033[31m", "\033[33m", "\033[1m", "\033[0m"
	}

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "plan", "apply":
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	autoApprove := false
	configPath := ""
	for _, a := range args {
		switch {
		case a == "-y" || a == "--auto-approve":
			autoApprove = true
		case a == "-h" || a == "--help":
			fmt.Print(usage)
			return
		case strings.HasPrefix(a, "-"):
			die("unknown option: %s", a)
		default:
			configPath = a
		}
	}
	if configPath == "" {
		die("no config file given")
	}

	baseURL := strings.TrimRight(os.Getenv("LP_API_URL"), "/")
	if baseURL == "" {
		die("LP_API_URL is not set")
	}
	protected := []string{"default"}
	if v, ok := os.LookupEnv("LP_PROTECTED_GROUPS"); ok {
		protected = strings.Fields(v)
	}

	roles, err := loadConfig(configPath)
	if err != nil {
		die("%v", err)
	}

	oauth := &clientcredentials.Config{
		ClientID:     os.Getenv("LP_CLIENT_ID"),
		ClientSecret: os.Getenv("LP_CLIENT_SECRET"),
		TokenURL:     os.Getenv("LP_TOKEN_URL"),
		Scopes:       strings.Fields(os.Getenv("LP_SCOPE")),
	}

	var missing []string
	for name, v := range map[string]string{
		"LP_TOKEN_URL": oauth.TokenURL, "LP_CLIENT_ID": oauth.ClientID, "LP_CLIENT_SECRET": oauth.ClientSecret,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		die("missing environment variables: %s", strings.Join(missing, ", "))
	}

	// Token requests use this client too, so they get the same timeout.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second})
	client := NewClient(ctx, baseURL, oauth)

	fmt.Printf("Refreshing state from %s...\n", baseURL)
	plan, err := buildPlan(client, roles, protected)
	if err != nil {
		die("%v", err)
	}
	if len(plan) == 0 {
		fmt.Printf("%sNo changes.%s LuckPerms matches the configuration.\n", green, reset)
		return
	}
	printPlan(plan)

	if cmd != "apply" {
		return
	}
	if !autoApprove {
		if !isTerminal(os.Stdin) {
			die("not running interactively; pass --auto-approve to apply")
		}
		fmt.Print("\nApply these changes? Only 'yes' will be accepted: ")
		answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && answer == "" {
			fmt.Println()
			die("no answer received; pass --auto-approve to apply non-interactively")
		}
		if strings.TrimSpace(answer) != "yes" {
			fmt.Println("Apply cancelled.")
			os.Exit(1)
		}
	}
	fmt.Println()
	if err := apply(client, plan); err != nil {
		die("apply failed: %v", err)
	}
	fmt.Printf("%sApply complete.%s\n", green, reset)
}
