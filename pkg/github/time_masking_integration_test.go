//go:build integration

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/github/github-mcp-server/pkg/translations"
	gogithub "github.com/google/go-github/v82/github"
	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture repository: PagerDuty/github-mcp-server-time-travel-fixtures
//
// This repo contains data at known timestamps for testing time-travel behavior
// against the real GitHub API. Do not modify the fixture repo.
//
// Data layout (all times UTC on 2026-03-24):
//
//   BEFORE CUTOFF:
//     18:38:39  Initial commit (d7dc74a) - creates README.md
//     18:39:22  "Add file before cutoff" (d7f15b1) - creates before-cutoff.txt on main
//     18:39:54  "Add feature file" (7943982) - creates feature.txt on feature-branch
//     18:39:58  Issue #1 created (open)
//     18:40:17  PR #2 created (open, feature-branch -> main)
//     18:40:29  Comment #1 on issue #1
//     18:41:01  Review #1 on PR #2 (COMMENTED)
//
//   --- CUTOFF: 18:45:00 ---
//
//   AFTER CUTOFF:
//     18:47:32  "Add file after cutoff" (571d4ce) - creates after-cutoff.txt on main
//     18:47:36  Comment #2 on issue #1
//     18:47:39  Issue #1 closed
//     18:50:16  Review #2 on PR #2 (COMMENTED)
//     18:50:20  PR #2 merged (67c393f)

const (
	fixtureOwner = "PagerDuty"
	fixtureRepo  = "github-mcp-server-time-travel-fixtures"

	// The cutoff sits between the "before" and "after" data points.
	fixtureCutoffStr = "2026-03-24T18:45:00Z"

	// SHA of the last commit on main before the cutoff.
	// This is "Add file before cutoff" - the SHA resolver should return this.
	fixtureBeforeCutoffSHA = "d7f15b1ec298d5f514a0150cabe986a631716362"

	// File that exists at the cutoff (created before).
	fixtureBeforeCutoffFile = "before-cutoff.txt"
	// File that does NOT exist at the cutoff (created after).
	fixtureAfterCutoffFile = "after-cutoff.txt"

	// Issue and PR numbers.
	fixtureIssueNumber = 1
	fixturePRNumber    = 2
)

var fixtureCutoff = mustParseTime(fixtureCutoffStr)

func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// integrationDeps creates BaseDeps with a real GitHub client for integration testing.
// Requires GITHUB_TOKEN environment variable.
func integrationDeps(t *testing.T, timeMasking *TimeMaskingState) BaseDeps {
	t.Helper()
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN not set, skipping integration test")
	}

	restClient := gogithub.NewClient(nil).WithAuthToken(token)
	gqlHTTPClient := &http.Client{
		Transport: &bearerTransport{token: token},
	}
	gqlClient := githubv4.NewClient(gqlHTTPClient)

	return BaseDeps{
		Client:      restClient,
		GQLClient:   gqlClient,
		T:           translations.NullTranslationHelper,
		TimeMasking: timeMasking,
	}
}

// bearerTransport adds Authorization header to requests.
type bearerTransport struct {
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

// --- SHA Override Tests ---

func TestTimeTravel_Integration_GetFileContents_HistoricalSHA(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)

	// Request before-cutoff.txt without specifying a ref/sha.
	// Time travel should resolve to the historical SHA and return the file.
	request := createMCPRequest(map[string]any{
		"owner": fixtureOwner,
		"repo":  fixtureRepo,
		"path":  fixtureBeforeCutoffFile,
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "before-cutoff.txt should be accessible at cutoff")
}

func TestTimeTravel_Integration_GetFileContents_AfterCutoffFile(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)

	// Request after-cutoff.txt without specifying a ref/sha.
	// At the cutoff SHA, this file doesn't exist yet - should get an error.
	request := createMCPRequest(map[string]any{
		"owner": fixtureOwner,
		"repo":  fixtureRepo,
		"path":  fixtureAfterCutoffFile,
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.True(t, result.IsError, "after-cutoff.txt should not exist at cutoff time")
}

// --- List Filtering Tests ---

func TestTimeTravel_Integration_ListCommits(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := ListCommits(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": fixtureOwner,
		"repo":  fixtureRepo,
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var commits []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &commits))

	// All commits should have dates <= cutoff
	for _, c := range commits {
		commit, ok := c["commit"].(map[string]any)
		require.True(t, ok)
		committer, ok := commit["committer"].(map[string]any)
		require.True(t, ok)
		dateStr, ok := committer["date"].(string)
		require.True(t, ok)
		commitDate, err := time.Parse(time.RFC3339, dateStr)
		require.NoError(t, err)
		assert.False(t, commitDate.After(fixtureCutoff),
			"commit %s has date %s which is after cutoff %s", c["sha"], dateStr, fixtureCutoffStr)
	}
	assert.GreaterOrEqual(t, len(commits), 2, "should have at least the initial commit and before-cutoff commit")
}

// --- Compound Tool Tests ---

func TestTimeTravel_Integration_IssueRead_StateMasking(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := IssueRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":       "get",
		"owner":        fixtureOwner,
		"repo":         fixtureRepo,
		"issue_number": float64(fixtureIssueNumber),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var issue map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &issue))

	// Issue was closed after cutoff, so state should be rolled back to open
	assert.Equal(t, "open", issue["state"], "issue should appear open at cutoff time")
	assert.Nil(t, issue["closed_at"], "closed_at should be null at cutoff time")
}

func TestTimeTravel_Integration_IssueRead_CommentFiltering(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := IssueRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":       "get_comments",
		"owner":        fixtureOwner,
		"repo":         fixtureRepo,
		"issue_number": float64(fixtureIssueNumber),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var comments []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &comments))

	assert.Len(t, comments, 1, "only the comment before cutoff should be visible")
	assert.Contains(t, comments[0]["body"], "before cutoff")
}

func TestTimeTravel_Integration_PullRequestRead_StateMasking(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := PullRequestRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":     "get",
		"owner":      fixtureOwner,
		"repo":       fixtureRepo,
		"pullNumber": float64(fixturePRNumber),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var pr map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &pr))

	// PR was merged after cutoff, so state should be rolled back to open
	assert.Equal(t, "open", pr["state"], "PR should appear open at cutoff time")
	assert.Equal(t, false, pr["merged"], "PR should not appear merged at cutoff time")
	assert.Nil(t, pr["merged_at"], "merged_at should be null at cutoff time")
}

func TestTimeTravel_Integration_PullRequestRead_ReviewFiltering(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := PullRequestRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":     "get_reviews",
		"owner":      fixtureOwner,
		"repo":       fixtureRepo,
		"pullNumber": float64(fixturePRNumber),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var reviews []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &reviews))

	assert.Len(t, reviews, 1, "only the review before cutoff should be visible")
	assert.Contains(t, reviews[0]["body"], "before cutoff")
}

// --- Write Blocking Test ---

func TestTimeTravel_Integration_WriteBlocked(t *testing.T) {
	state := NewTimeMaskingState()
	state.SetCutoff(&fixtureCutoff)
	deps := integrationDeps(t, state)

	serverTool := IssueWrite(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method": "create",
		"owner":  fixtureOwner,
		"repo":   fixtureRepo,
		"title":  "This should be blocked",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.True(t, result.IsError, "write tools should be blocked during time travel")
	errorContent := getErrorResult(t, result)
	assert.Contains(t, errorContent.Text, "write operations are blocked")
}

// --- Round-Trip Test ---

func TestTimeTravel_Integration_RoundTrip(t *testing.T) {
	state := NewTimeMaskingState()
	deps := integrationDeps(t, state)

	serverTool := IssueRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":       "get_comments",
		"owner":        fixtureOwner,
		"repo":         fixtureRepo,
		"issue_number": float64(fixtureIssueNumber),
	})

	// Without time travel: should see both comments
	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	textContent := getTextResult(t, result)
	var allComments []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &allComments))
	assert.Len(t, allComments, 2, "without time travel, both comments should be visible")

	// Enable time travel: should see only 1 comment
	state.SetCutoff(&fixtureCutoff)
	result, err = handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	textContent = getTextResult(t, result)
	var filteredComments []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &filteredComments))
	assert.Len(t, filteredComments, 1, "with time travel, only 1 comment should be visible")

	// Disable time travel: should see both again
	state.SetCutoff(nil)
	result, err = handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	textContent = getTextResult(t, result)
	var restoredComments []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &restoredComments))
	assert.Len(t, restoredComments, 2, "after disabling time travel, both comments should be visible again")
}
