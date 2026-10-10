package huly

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// defaultTimeout is the network timeout for every Huly request.
const defaultTimeout = 30 * time.Second

// maxErrorBody caps how much of an error response is read for a diagnostic.
const maxErrorBody = 512

// serverConfig is the subset of the front's /config.json the adapter reads.
type serverConfig struct {
	AccountsURL     string `json:"ACCOUNTS_URL"`
	CollaboratorURL string `json:"COLLABORATOR_URL"`
}

// workspaceLogin is the selectWorkspace result: a workspace-scoped token
// and the transactor endpoint that accepts it.
type workspaceLogin struct {
	Endpoint     string `json:"endpoint"`
	Token        string `json:"token"`
	Workspace    string `json:"workspace"`
	WorkspaceURL string `json:"workspaceUrl"`
}

// account is the transactor's view of the caller.
type account struct {
	UUID            string `json:"uuid"`
	PrimarySocialID string `json:"primarySocialId"`
}

// findResult is the wire envelope of a find-all response.
type findResult struct {
	DataType string            `json:"dataType"`
	Total    int               `json:"total"`
	Value    []json.RawMessage `json:"value"`
}

// rpcClient is the transport the adapter drives. The production
// implementation talks HTTP; tests substitute a fake.
type rpcClient interface {
	// FindAll returns every document of class matching query.
	FindAll(ctx context.Context, class string, query map[string]any, options map[string]any) ([]json.RawMessage, error)

	// Tx applies one transaction.
	Tx(ctx context.Context, tx map[string]any) error

	// GetMarkup returns the serialized markup of a collaborative document
	// attribute, read from the blob source.
	GetMarkup(ctx context.Context, objectClass, objectID, objectAttr, source string) (string, error)
}

// httpRPC is the production [rpcClient]: the transactor REST API and the
// collaborator RPC endpoint, both authorized by a workspace token.
type httpRPC struct {
	transactor   *httpkit.Client
	collaborator *httpkit.Client
	workspace    string
}

// connection is everything the constructor learns while logging in.
type connection struct {
	rpc          *httpRPC
	login        workspaceLogin
	socialID     string
	frontBaseURL string
}

// connect resolves the server configuration, exchanges the token for a
// workspace token, and reads the caller's account.
func connect(ctx context.Context, frontURL, token, workspace, userAgent string) (*connection, error) {
	frontURL = strings.TrimRight(frontURL, "/")

	front := newHTTPClient(frontURL, "", userAgent)
	body, _, err := front.Get(ctx, "/config.json", nil)
	if err != nil {
		return nil, err
	}

	var cfg serverConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, payloadError("decode /config.json", err)
	}
	if cfg.AccountsURL == "" {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: /config.json carries no ACCOUNTS_URL"}
	}

	accounts := newHTTPClient(resolveURL(frontURL, cfg.AccountsURL), token, userAgent)
	var login workspaceLogin
	if err := accountRPC(ctx, accounts, "selectWorkspace", map[string]any{
		"workspaceUrl": workspace,
		"kind":         "external",
	}, &login); err != nil {
		return nil, err
	}
	if login.Token == "" || login.Workspace == "" || login.Endpoint == "" {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: selectWorkspace returned an incomplete login"}
	}
	// selectWorkspace falls back to some workspace of the account when the
	// requested name matches none, so confirm the one it picked.
	if workspace != "" && !strings.EqualFold(login.WorkspaceURL, workspace) {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("huly: workspace %q not found; the token selected %q", workspace, login.WorkspaceURL),
		}
	}

	rpc := &httpRPC{
		transactor:   newHTTPClient(httpURL(resolveURL(frontURL, login.Endpoint)), login.Token, userAgent),
		collaborator: newHTTPClient(httpURL(resolveURL(frontURL, cfg.CollaboratorURL)), login.Token, userAgent),
		workspace:    login.Workspace,
	}

	body, _, err = rpc.transactor.Get(ctx, "/api/v1/account/"+url.PathEscape(login.Workspace), nil)
	if err != nil {
		return nil, err
	}
	var acct account
	if err := json.Unmarshal(body, &acct); err != nil {
		return nil, payloadError("decode account", err)
	}

	return &connection{rpc: rpc, login: login, socialID: acct.PrimarySocialID, frontBaseURL: frontURL}, nil
}

// accountRPC calls one method of the account service's JSON-RPC endpoint.
func accountRPC(ctx context.Context, client *httpkit.Client, method string, params map[string]any, out any) error {
	raw, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return payloadError("encode account request", err)
	}

	body, err := client.Send(ctx, http.MethodPost, "", bytes.NewReader(raw))
	if err != nil {
		return err
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *platformStatus `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return payloadError("decode account response", err)
	}
	if envelope.Error != nil {
		return envelope.Error.trackerError("huly account " + method)
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return payloadError("decode account result", err)
	}
	return nil
}

// platformStatus is Huly's error status shape.
type platformStatus struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"`
	Params   map[string]any `json:"params"`
}

func (s *platformStatus) trackerError(op string) error {
	kind := domain.ErrTrackerAPI
	switch {
	case strings.HasSuffix(s.Code, ":Unauthorized"), strings.HasSuffix(s.Code, ":Forbidden"),
		strings.HasSuffix(s.Code, ":AccountNotFound"), strings.HasSuffix(s.Code, ":InvalidPassword"):
		kind = domain.ErrTrackerAuth
	case strings.HasSuffix(s.Code, ":WorkspaceNotFound"), strings.HasSuffix(s.Code, ":NotFound"):
		kind = domain.ErrTrackerNotFound
	case strings.HasSuffix(s.Code, ":BadRequest"):
		kind = domain.ErrTrackerPayload
	}
	return &domain.TrackerError{Kind: kind, Message: fmt.Sprintf("%s: %s", op, s.Code)}
}

func (c *httpRPC) FindAll(ctx context.Context, class string, query map[string]any, options map[string]any) ([]json.RawMessage, error) {
	if options == nil {
		options = map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"_class": class, "query": query, "options": options})
	if err != nil {
		return nil, payloadError("encode find-all request", err)
	}

	body, err := c.transactor.Send(ctx, http.MethodPost, "/api/v1/find-all/"+url.PathEscape(c.workspace), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	var result findResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, payloadError("decode find-all response", err)
	}
	if result.Value == nil {
		return []json.RawMessage{}, nil
	}
	return result.Value, nil
}

func (c *httpRPC) Tx(ctx context.Context, tx map[string]any) error {
	raw, err := json.Marshal(tx)
	if err != nil {
		return payloadError("encode tx", err)
	}
	_, err = c.transactor.Send(ctx, http.MethodPost, "/api/v1/tx/"+url.PathEscape(c.workspace), bytes.NewReader(raw))
	return err
}

func (c *httpRPC) GetMarkup(ctx context.Context, objectClass, objectID, objectAttr, source string) (string, error) {
	documentID := strings.Join([]string{c.workspace, objectClass, objectID, objectAttr}, "|")
	raw, err := json.Marshal(map[string]any{
		"method":  "getContent",
		"payload": map[string]any{"source": source},
	})
	if err != nil {
		return "", payloadError("encode collaborator request", err)
	}

	// Escape the way JavaScript's encodeURIComponent does, which the
	// collaborator decodes: every reserved character, spaces as %20.
	escaped := strings.ReplaceAll(url.QueryEscape(documentID), "+", "%20")
	body, err := c.collaborator.Send(ctx, http.MethodPost, "/rpc/"+escaped, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}

	var result struct {
		Content map[string]string `json:"content"`
		Error   string            `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", payloadError("decode collaborator response", err)
	}
	if result.Error != "" {
		return "", &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "huly collaborator: " + result.Error}
	}
	return result.Content[objectAttr], nil
}

func newHTTPClient(baseURL, token, userAgent string) *httpkit.Client {
	return httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: defaultTimeout,
		Authorize: func(req *http.Request) {
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyResponse,
		ClassifyTransport: httpkit.ClassifyTransport,
	})
}

// classifyResponse maps a non-2xx HTTP response to a [*domain.TrackerError].
func classifyResponse(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)

	kind := domain.ErrTrackerAPI
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		kind = domain.ErrTrackerAuth
	case resp.StatusCode == http.StatusNotFound:
		kind = domain.ErrTrackerNotFound
	case resp.StatusCode == http.StatusBadRequest:
		kind = domain.ErrTrackerPayload
	case resp.StatusCode >= http.StatusInternalServerError:
		kind = domain.ErrTrackerTransport
	}

	return &domain.TrackerError{
		Kind:    kind,
		Message: fmt.Sprintf("huly: %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(snippet))),
		Status:  resp.StatusCode,
	}
}

func payloadError(what string, err error) error {
	return &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "huly: " + what, Err: err}
}

// resolveURL resolves ref against base, so a relative URL in the server
// configuration resolves against the front URL.
func resolveURL(base, ref string) string {
	b, err := url.Parse(base + "/")
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// httpURL rewrites a WebSocket URL to its HTTP equivalent.
func httpURL(raw string) string {
	switch {
	case strings.HasPrefix(raw, "wss://"):
		return "https://" + strings.TrimPrefix(raw, "wss://")
	case strings.HasPrefix(raw, "ws://"):
		return "http://" + strings.TrimPrefix(raw, "ws://")
	default:
		return raw
	}
}

// generateID returns a new document id in Huly's 24-hex-digit form: four
// bytes of Unix time followed by eight random bytes.
func generateID() string {
	var b [12]byte
	binary.BigEndian.PutUint32(b[:4], uint32(time.Now().Unix())) //nolint:gosec // Unix seconds fit in 32 bits until 2106
	_, _ = rand.Read(b[4:])
	return hex.EncodeToString(b[:])
}
