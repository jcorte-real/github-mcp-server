package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	gogithub "github.com/google/go-github/v82/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TimeMaskingState holds the time-travel cutoff and cached SHA resolutions.
// It is stored on BaseDeps and shared across all tool calls in a session.
type TimeMaskingState struct {
	mu       sync.RWMutex
	cutoff   *time.Time
	shaCache map[string]string // "owner/repo" -> commit SHA at cutoff
}

// NewTimeMaskingState creates a new TimeMaskingState with no cutoff set.
func NewTimeMaskingState() *TimeMaskingState {
	return &TimeMaskingState{
		shaCache: make(map[string]string),
	}
}

// SetCutoff sets the time-travel cutoff. Pass nil to disable time travel.
// Clears the SHA cache since a new cutoff means different historical state.
func (s *TimeMaskingState) SetCutoff(t *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cutoff = t
	s.shaCache = make(map[string]string)
}

// GetCutoff returns the current cutoff, or nil if time travel is not active.
func (s *TimeMaskingState) GetCutoff() *time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cutoff
}

// GetOrResolveSHA returns the cached commit SHA for the given repo at the
// current cutoff, or resolves it by calling ListCommits(until=cutoff, per_page=1).
func (s *TimeMaskingState) GetOrResolveSHA(ctx context.Context, client *gogithub.Client, owner, repo string) (string, error) {
	key := owner + "/" + repo

	s.mu.RLock()
	if sha, ok := s.shaCache[key]; ok {
		s.mu.RUnlock()
		return sha, nil
	}
	cutoff := s.cutoff
	s.mu.RUnlock()

	if cutoff == nil {
		return "", fmt.Errorf("time travel is not active")
	}

	commits, _, err := client.Repositories.ListCommits(ctx, owner, repo, &gogithub.CommitsListOptions{
		Until: *cutoff,
		ListOptions: gogithub.ListOptions{
			PerPage: 1,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to resolve historical SHA for %s: %w", key, err)
	}
	if len(commits) == 0 {
		return "", fmt.Errorf("no commits found before %s for %s", cutoff.Format(time.RFC3339), key)
	}

	sha := commits[0].GetSHA()

	s.mu.Lock()
	s.shaCache[key] = sha
	s.mu.Unlock()

	return sha, nil
}

// TimeFilterConfig describes how to filter a tool's JSON response by time.
type TimeFilterConfig struct {
	// TimestampKey is the JSON field name containing the timestamp (e.g. "created_at").
	TimestampKey string
	// ArrayKey is the JSON field containing the array to filter (e.g. "items").
	// Empty string means the response is a top-level array.
	ArrayKey string
	// StateFields are timestamp fields to null out if their value > cutoff,
	// along with associated state changes (e.g. merged_at -> state:"open").
	StateFields []StateMaskField
}

// StateMaskField describes a timestamp field that, when > cutoff, should be
// nulled out, along with related fields to null and state overrides.
type StateMaskField struct {
	// TimestampKey is the field to check (e.g. "merged_at", "closed_at").
	TimestampKey string
	// NullFields are additional fields to null when masking (e.g. "merge_commit_sha").
	NullFields []string
	// StateOverride sets "state" to this value when masking (e.g. "open").
	StateOverride string
	// FieldOverrides sets arbitrary fields to specific JSON values when masking.
	FieldOverrides map[string]any
}

// toolTimeFilters maps tool names to their client-side filter configurations.
var toolTimeFilters = map[string]TimeFilterConfig{
	"list_pull_requests":                      {TimestampKey: "created_at"},
	"list_releases":                           {TimestampKey: "created_at"},
	"list_issues":                             {TimestampKey: "created_at"},
	"list_code_scanning_alerts":               {TimestampKey: "created_at"},
	"list_secret_scanning_alerts":             {TimestampKey: "created_at"},
	"list_dependabot_alerts":                  {TimestampKey: "created_at"},
	"list_discussions":                        {TimestampKey: "createdAt"},
	"list_gists":                              {TimestampKey: "created_at"},
	"projects_list":                           {TimestampKey: "createdAt"},
	"list_repository_security_advisories":     {TimestampKey: "published_at"},
	"list_org_repository_security_advisories": {TimestampKey: "published_at"},
	"list_starred_repositories":               {TimestampKey: "starred_at"},
}

// compoundToolTimeFilters maps method names within compound tools to their
// filter configurations. These are applied inside compound tool handlers
// (issue_read, pull_request_read) since different methods produce different
// response shapes with different timestamp keys.
var compoundToolTimeFilters = map[string]TimeFilterConfig{
	"get_comments": {TimestampKey: "created_at"},
	"get_reviews":  {TimestampKey: "submitted_at"},
}

// prStateMaskFields defines state masking for pull requests: when merged_at
// or closed_at is after the cutoff, roll back to open state.
var prStateMaskFields = []StateMaskField{
	{
		TimestampKey:   "merged_at",
		NullFields:     []string{"merge_commit_sha"},
		StateOverride:  "open",
		FieldOverrides: map[string]any{"merged": false},
	},
	{
		TimestampKey:  "closed_at",
		StateOverride: "open",
	},
}

// issueStateMaskFields defines state masking for issues: when closed_at
// is after the cutoff, roll back to open state.
var issueStateMaskFields = []StateMaskField{
	{
		TimestampKey:  "closed_at",
		NullFields:    []string{"closed_by", "state_reason"},
		StateOverride: "open",
	},
}

// toolExistenceChecks maps tool names to the timestamp field used for
// single-item existence checks. If the item's timestamp > cutoff, the
// tool returns a "not found" error.
var toolExistenceChecks = map[string]string{
	"get_commit":               "commit.committer.date",
	"get_gist":                 "created_at",
	"get_code_scanning_alert":  "created_at",
	"get_secret_scanning_alert": "created_at",
	"get_dependabot_alert":     "created_at",
	"get_release_by_tag":       "created_at",
	"get_latest_release":       "created_at",
}

// FilterResponseByTime filters a tool's JSON text result by the given cutoff.
// It handles three response shapes:
//   - Top-level JSON array: filters items where timestampKey > cutoff
//   - Object with embedded array (via ArrayKey): filters the nested array
//   - Single object: returns error result if timestampKey > cutoff
//
// Returns nil if no filtering was applied (pass-through).
func FilterResponseByTime(result *mcp.CallToolResult, cutoff time.Time, config TimeFilterConfig) *mcp.CallToolResult {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return nil
	}

	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		return nil
	}

	jsonText := textContent.Text

	// Try to determine the shape of the response
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(jsonText), &raw); err != nil {
		return nil
	}

	// Check if it's an array
	if len(jsonText) > 0 && jsonText[0] == '[' {
		filtered := filterArray([]byte(jsonText), cutoff, config.TimestampKey)
		return utils.NewToolResultText(string(filtered))
	}

	// It's an object
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(jsonText), &obj); err != nil {
		return nil
	}

	// If ArrayKey is set, filter the embedded array
	if config.ArrayKey != "" {
		arrData, exists := obj[config.ArrayKey]
		if !exists {
			return nil
		}
		filtered := filterArray(arrData, cutoff, config.TimestampKey)
		obj[config.ArrayKey] = filtered
		out, err := json.Marshal(obj)
		if err != nil {
			return nil
		}
		return utils.NewToolResultText(string(out))
	}

	// Single object: check existence
	if !isBeforeCutoff(obj, cutoff, config.TimestampKey) {
		return utils.NewToolResultError("item does not exist at the specified time travel cutoff")
	}

	// Apply state masking if configured
	if len(config.StateFields) > 0 {
		masked := applyStateMasking(obj, cutoff, config.StateFields)
		out, err := json.Marshal(masked)
		if err != nil {
			return nil
		}
		return utils.NewToolResultText(string(out))
	}

	return nil
}

// CheckExistence checks if a single-item response existed at the cutoff.
// Returns an error result if the item didn't exist, nil otherwise.
func CheckExistence(result *mcp.CallToolResult, cutoff time.Time, timestampPath string) *mcp.CallToolResult {
	if result == nil || result.IsError || len(result.Content) == 0 {
		return nil
	}

	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		return nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(textContent.Text), &obj); err != nil {
		return nil
	}

	if !isBeforeCutoff(obj, cutoff, timestampPath) {
		return utils.NewToolResultError("item does not exist at the specified time travel cutoff")
	}

	return nil
}

// filterArray removes items from a JSON array where the timestamp field > cutoff.
func filterArray(data []byte, cutoff time.Time, timestampKey string) json.RawMessage {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return data
	}

	var filtered []json.RawMessage
	for _, item := range items {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil {
			filtered = append(filtered, item)
			continue
		}
		if isBeforeCutoff(obj, cutoff, timestampKey) {
			filtered = append(filtered, item)
		}
	}

	if filtered == nil {
		filtered = []json.RawMessage{}
	}

	out, err := json.Marshal(filtered)
	if err != nil {
		return data
	}
	return out
}

// isBeforeCutoff checks if the timestamp at the given key is <= cutoff.
// Returns true if the field is missing or cannot be parsed (conservative).
func isBeforeCutoff(obj map[string]json.RawMessage, cutoff time.Time, timestampKey string) bool {
	tsRaw, exists := obj[timestampKey]
	if !exists {
		return true
	}

	var tsStr string
	if err := json.Unmarshal(tsRaw, &tsStr); err != nil {
		return true
	}

	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return true
	}

	return !ts.After(cutoff)
}

// applyStateMasking nulls out timestamp fields and applies state overrides
// when those timestamps are after the cutoff.
func applyStateMasking(obj map[string]json.RawMessage, cutoff time.Time, fields []StateMaskField) map[string]json.RawMessage {
	for _, f := range fields {
		if isBeforeCutoff(obj, cutoff, f.TimestampKey) {
			continue
		}
		// Null the timestamp field
		obj[f.TimestampKey] = json.RawMessage("null")
		// Null related fields
		for _, nf := range f.NullFields {
			obj[nf] = json.RawMessage("null")
		}
		// Override state
		if f.StateOverride != "" {
			stateJSON, _ := json.Marshal(f.StateOverride)
			obj["state"] = json.RawMessage(stateJSON)
		}
		// Apply field overrides
		for k, v := range f.FieldOverrides {
			valJSON, _ := json.Marshal(v)
			obj[k] = json.RawMessage(valJSON)
		}
	}
	return obj
}

// ApplyTimeMaskingToResult applies time-travel filtering to a compound tool's
// result based on the method name. For "get" methods on PRs/issues, it applies
// state masking. For list-like sub-methods (get_comments, get_reviews), it
// filters the array by timestamp. Returns the original result if no filtering
// is needed.
func ApplyTimeMaskingToResult(result *mcp.CallToolResult, deps ToolDependencies, method string, stateMaskFields []StateMaskField) *mcp.CallToolResult {
	if result == nil || result.IsError {
		return result
	}
	tm := deps.GetTimeMasking()
	if tm == nil {
		return result
	}
	cutoff := tm.GetCutoff()
	if cutoff == nil {
		return result
	}

	if method == "get" && len(stateMaskFields) > 0 {
		config := TimeFilterConfig{
			TimestampKey: "created_at",
			StateFields:  stateMaskFields,
		}
		if filtered := FilterResponseByTime(result, *cutoff, config); filtered != nil {
			return filtered
		}
		return result
	}

	if filterCfg, ok := compoundToolTimeFilters[method]; ok {
		if filtered := FilterResponseByTime(result, *cutoff, filterCfg); filtered != nil {
			return filtered
		}
	}

	return result
}

// SetTimeTravel creates the set_time_travel tool for controlling time masking.
func SetTimeTravel(t translations.TranslationHelperFunc, state *TimeMaskingState) mcp.Tool {
	return mcp.Tool{
		Name:        "set_time_travel",
		Description: t("TOOL_SET_TIME_TRAVEL_DESCRIPTION", "Set or clear a time-travel cutoff for regression testing. When active, all tools return only data that existed at or before the cutoff timestamp. Write operations are blocked. Pass an empty cutoff string to disable."),
		Annotations: &mcp.ToolAnnotations{
			Title:        t("TOOL_SET_TIME_TRAVEL_USER_TITLE", "Set time travel cutoff"),
			ReadOnlyHint: true,
		},
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"cutoff": {
					Type:        "string",
					Description: "ISO 8601 timestamp to travel to (e.g. '2024-01-15T10:30:00Z'). Empty string disables time travel.",
				},
			},
			Required: []string{"cutoff"},
		},
	}
}

// SetTimeTravelHandler returns the handler function for the set_time_travel tool.
func SetTimeTravelHandler(state *TimeMaskingState) func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return utils.NewToolResultError(fmt.Sprintf("failed to parse arguments: %s", err)), nil
		}

		cutoffStr, err := OptionalParam[string](args, "cutoff")
		if err != nil {
			return utils.NewToolResultError(err.Error()), nil
		}

		if cutoffStr == "" {
			state.SetCutoff(nil)
			return utils.NewToolResultText("Time travel disabled."), nil
		}

		cutoff, err := time.Parse(time.RFC3339, cutoffStr)
		if err != nil {
			return utils.NewToolResultError(fmt.Sprintf("invalid cutoff timestamp: %s. Expected ISO 8601 format (e.g. '2024-01-15T10:30:00Z')", err)), nil
		}

		state.SetCutoff(&cutoff)
		return utils.NewToolResultText(fmt.Sprintf("Time travel active. Cutoff set to %s. All tools will now return only data from before this time. Write operations are blocked.", cutoff.Format(time.RFC3339))), nil
	}
}

// IsWriteBlocked checks if a tool should be blocked due to time travel being active.
// Returns true if the tool is a write tool and time travel is active.
func IsWriteBlocked(tool mcp.Tool, state *TimeMaskingState) bool {
	if state == nil || state.GetCutoff() == nil {
		return false
	}
	if tool.Annotations == nil {
		// No annotations means we can't determine read-only status; block by default
		return true
	}
	return !tool.Annotations.ReadOnlyHint
}
