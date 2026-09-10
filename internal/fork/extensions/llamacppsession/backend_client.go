package llamacppsession

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/fork/extension"
)

type slotInfo struct {
	ID           int  `json:"id"`
	IsProcessing bool `json:"is_processing"`
}

// backendClient isolates the small llama.cpp slot-control API used by the
// allocator. Its timeout is additionally bounded by each lifecycle or request
// context.
type backendClient struct {
	httpClient *http.Client
	timeout    time.Duration
}

func newBackendClient(timeout time.Duration) *backendClient {
	return &backendClient{httpClient: &http.Client{}, timeout: timeout}
}

// slots discovers generation-local resources only after the process health
// check; callers still validate that the returned slots are safe to adopt.
func (c *backendClient) slots(ctx context.Context, runtime extension.BackendRuntime) ([]slotInfo, error) {
	requestCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, backendURL(runtime.BaseURL, "slots", nil), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /slots: %w", err)
	}
	defer resp.Body.Close()
	if err := requireSuccess(resp); err != nil {
		return nil, fmt.Errorf("GET /slots: %w", err)
	}
	var slots []slotInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&slots); err != nil {
		return nil, fmt.Errorf("decode /slots: %w", err)
	}
	return slots, nil
}

// action performs one save, restore, or erase operation against an explicit
// slot so backend-specific protocol details stay out of allocation code.
func (c *backendClient) action(ctx context.Context, runtime extension.BackendRuntime, slot int, action, filename string) error {
	requestCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	body := map[string]string{}
	if filename != "" {
		body["filename"] = filename
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	query := url.Values{"action": []string{action}}
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, backendURL(runtime.BaseURL, path.Join("slots", strconv.Itoa(slot)), query), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slot %d %s: %w", slot, action, err)
	}
	defer resp.Body.Close()
	if err := requireSuccess(resp); err != nil {
		return fmt.Errorf("slot %d %s: %w", slot, action, err)
	}
	return nil
}

func (c *backendClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

func backendURL(base *url.URL, suffix string, query url.Values) string {
	u := *base
	u.Path = path.Join(strings.TrimSuffix(u.Path, "/"), suffix)
	u.RawPath = ""
	u.RawQuery = query.Encode()
	return u.String()
}

func requireSuccess(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return fmt.Errorf("backend returned %s: %s", resp.Status, strings.TrimSpace(string(raw)))
}
