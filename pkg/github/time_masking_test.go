package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/github/github-mcp-server/internal/toolsnaps"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/google/go-github/v82/github"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- TimeMaskingState unit tests ---

func Test_TimeMaskingState_SetAndGetCutoff(t *testing.T) {
	state := NewTimeMaskingState()

	// Initially nil
	assert.Nil(t, state.GetCutoff())

	// Set a cutoff
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state.SetCutoff(&cutoff)
	got := state.GetCutoff()
	require.NotNil(t, got)
	assert.Equal(t, cutoff, *got)
}

func Test_TimeMaskingState_SetCutoffNilDisables(t *testing.T) {
	state := NewTimeMaskingState()
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state.SetCutoff(&cutoff)
	require.NotNil(t, state.GetCutoff())

	state.SetCutoff(nil)
	assert.Nil(t, state.GetCutoff())
}

func Test_TimeMaskingState_SetCutoffClearsSHACache(t *testing.T) {
	state := NewTimeMaskingState()
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state.SetCutoff(&cutoff)

	// Manually inject a cached SHA
	state.mu.Lock()
	state.shaCache["owner/repo"] = "abc123"
	state.mu.Unlock()

	// Change cutoff
	newCutoff := time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC)
	state.SetCutoff(&newCutoff)

	// Cache should be cleared
	state.mu.RLock()
	_, exists := state.shaCache["owner/repo"]
	state.mu.RUnlock()
	assert.False(t, exists, "SHA cache should be cleared after cutoff change")
}

func Test_TimeMaskingState_GetOrResolveSHA_CallsListCommits(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	expectedSHA := "abc123def456"

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: expectQueryParams(t, map[string]string{
			"until":    cutoff.Format(time.RFC3339),
			"per_page": "1",
		}).andThen(mockResponse(t, http.StatusOK, []*github.RepositoryCommit{
			{SHA: github.Ptr(expectedSHA)},
		})),
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	sha, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo")
	require.NoError(t, err)
	assert.Equal(t, expectedSHA, sha)
}

func Test_TimeMaskingState_GetOrResolveSHA_CachesResult(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	expectedSHA := "abc123def456"
	callCount := 0

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal([]*github.RepositoryCommit{
				{SHA: github.Ptr(expectedSHA)},
			})
			_, _ = w.Write(body)
		},
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	// First call resolves
	sha1, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo")
	require.NoError(t, err)
	assert.Equal(t, expectedSHA, sha1)

	// Second call should use cache
	sha2, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo")
	require.NoError(t, err)
	assert.Equal(t, expectedSHA, sha2)
	assert.Equal(t, 1, callCount, "ListCommits should only be called once due to caching")
}

func Test_TimeMaskingState_GetOrResolveSHA_ErrorOnEmptyCommits(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.RepositoryCommit{}),
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	_, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no commits found before")
}

// --- set_time_travel tool tests ---

func Test_SetTimeTravel_Schema(t *testing.T) {
	state := NewTimeMaskingState()
	tool := SetTimeTravel(translations.NullTranslationHelper, state)
	require.NoError(t, toolsnaps.Test(tool.Name, tool))

	assert.Equal(t, "set_time_travel", tool.Name)
	assert.NotEmpty(t, tool.Description)
	assert.True(t, tool.Annotations.ReadOnlyHint)
}

func Test_SetTimeTravel_ValidCutoff(t *testing.T) {
	state := NewTimeMaskingState()
	handler := SetTimeTravelHandler(state)

	req := createMCPRequest(map[string]any{
		"cutoff": "2024-06-15T10:30:00Z",
	})
	result, err := handler(context.Background(), &req)

	require.NoError(t, err)
	require.False(t, result.IsError)
	textContent := getTextResult(t, result)
	assert.Contains(t, textContent.Text, "Time travel active")
	assert.Contains(t, textContent.Text, "2024-06-15T10:30:00Z")

	cutoff := state.GetCutoff()
	require.NotNil(t, cutoff)
	assert.Equal(t, time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC), *cutoff)
}

func Test_SetTimeTravel_EmptyDisables(t *testing.T) {
	state := NewTimeMaskingState()
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state.SetCutoff(&cutoff)

	handler := SetTimeTravelHandler(state)
	req := createMCPRequest(map[string]any{
		"cutoff": "",
	})
	result, err := handler(context.Background(), &req)

	require.NoError(t, err)
	require.False(t, result.IsError)
	textContent := getTextResult(t, result)
	assert.Contains(t, textContent.Text, "disabled")
	assert.Nil(t, state.GetCutoff())
}

func Test_SetTimeTravel_InvalidTimestamp(t *testing.T) {
	state := NewTimeMaskingState()
	handler := SetTimeTravelHandler(state)

	req := createMCPRequest(map[string]any{
		"cutoff": "not-a-date",
	})
	result, err := handler(context.Background(), &req)

	require.NoError(t, err)
	require.True(t, result.IsError)
	errorContent := getErrorResult(t, result)
	assert.Contains(t, errorContent.Text, "invalid cutoff timestamp")
}

func Test_SetTimeTravel_UpdatesCutoff(t *testing.T) {
	state := NewTimeMaskingState()
	handler := SetTimeTravelHandler(state)

	// Set first cutoff
	req1 := createMCPRequest(map[string]any{
		"cutoff": "2024-06-15T10:00:00Z",
	})
	result1, err := handler(context.Background(), &req1)
	require.NoError(t, err)
	require.False(t, result1.IsError)

	first := state.GetCutoff()
	require.NotNil(t, first)
	assert.Equal(t, time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC), *first)

	// Update cutoff
	req2 := createMCPRequest(map[string]any{
		"cutoff": "2024-07-01T12:00:00Z",
	})
	result2, err := handler(context.Background(), &req2)
	require.NoError(t, err)
	require.False(t, result2.IsError)

	second := state.GetCutoff()
	require.NotNil(t, second)
	assert.Equal(t, time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC), *second)
}

// --- Write blocking tests ---

func Test_IsWriteBlocked_WriteToolBlocked(t *testing.T) {
	state := NewTimeMaskingState()
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state.SetCutoff(&cutoff)

	tool := mcp.Tool{
		Name: "create_issue",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: false,
		},
	}
	assert.True(t, IsWriteBlocked(tool, state))
}

func Test_IsWriteBlocked_WriteToolAllowedWhenInactive(t *testing.T) {
	state := NewTimeMaskingState()

	tool := mcp.Tool{
		Name: "create_issue",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: false,
		},
	}
	assert.False(t, IsWriteBlocked(tool, state))
}

func Test_IsWriteBlocked_ReadToolAllowedWhenActive(t *testing.T) {
	state := NewTimeMaskingState()
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state.SetCutoff(&cutoff)

	tool := mcp.Tool{
		Name: "get_file_contents",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}
	assert.False(t, IsWriteBlocked(tool, state))
}

func Test_IsWriteBlocked_NilStateAllowed(t *testing.T) {
	tool := mcp.Tool{
		Name: "create_issue",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: false,
		},
	}
	assert.False(t, IsWriteBlocked(tool, nil))
}

// --- SHA resolver tests ---

func Test_GetOrResolveSHA_PassesCorrectParams(t *testing.T) {
	cutoff := time.Date(2024, 3, 15, 14, 30, 0, 0, time.UTC)

	var capturedURL string
	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: func(w http.ResponseWriter, r *http.Request) {
			capturedURL = r.URL.String()
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal([]*github.RepositoryCommit{
				{SHA: github.Ptr("sha123")},
			})
			_, _ = w.Write(body)
		},
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	_, err := state.GetOrResolveSHA(context.Background(), client, "myorg", "myrepo")
	require.NoError(t, err)

	assert.Contains(t, capturedURL, "until=2024-03-15T14")
	assert.Contains(t, capturedURL, "per_page=1")
}

func Test_GetOrResolveSHA_DifferentReposSeparateCalls(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	callCount := 0

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: func(w http.ResponseWriter, r *http.Request) {
			callCount++
			sha := fmt.Sprintf("sha-for-call-%d", callCount)
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal([]*github.RepositoryCommit{
				{SHA: github.Ptr(sha)},
			})
			_, _ = w.Write(body)
		},
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	sha1, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo-a")
	require.NoError(t, err)

	sha2, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo-b")
	require.NoError(t, err)

	assert.NotEqual(t, sha1, sha2, "different repos should resolve different SHAs")
	assert.Equal(t, 2, callCount, "should make separate API calls for different repos")
}

func Test_GetOrResolveSHA_APIFailure(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: mockResponse(t, http.StatusNotFound, `{"message": "Not Found"}`),
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	_, err := state.GetOrResolveSHA(context.Background(), client, "owner", "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to resolve historical SHA")
}

// --- Generic response filter tests ---

func Test_FilterResponseByTime_FiltersTopLevelArray(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `[
		{"id": 1, "created_at": "2024-06-10T00:00:00Z"},
		{"id": 2, "created_at": "2024-06-20T00:00:00Z"},
		{"id": 3, "created_at": "2024-06-14T23:59:59Z"}
	]`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(result, cutoff, config)
	require.NotNil(t, filtered)
	require.False(t, filtered.IsError)

	textContent := getTextResult(t, filtered)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &items))
	assert.Len(t, items, 2)
	assert.Equal(t, float64(1), items[0]["id"])
	assert.Equal(t, float64(3), items[1]["id"])
}

func Test_FilterResponseByTime_FiltersEmbeddedArray(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{
		"total_count": 3,
		"items": [
			{"id": 1, "created_at": "2024-06-10T00:00:00Z"},
			{"id": 2, "created_at": "2024-06-20T00:00:00Z"},
			{"id": 3, "created_at": "2024-06-01T00:00:00Z"}
		]
	}`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{TimestampKey: "created_at", ArrayKey: "items"}

	filtered := FilterResponseByTime(result, cutoff, config)
	require.NotNil(t, filtered)
	require.False(t, filtered.IsError)

	textContent := getTextResult(t, filtered)
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &obj))

	var items []map[string]any
	require.NoError(t, json.Unmarshal(obj["items"], &items))
	assert.Len(t, items, 2)
	assert.Equal(t, float64(1), items[0]["id"])
	assert.Equal(t, float64(3), items[1]["id"])
}

func Test_FilterResponseByTime_SingleObjectPassesWhenBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{"id": 1, "created_at": "2024-06-10T00:00:00Z"}`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(result, cutoff, config)
	// nil means pass-through (no filtering needed)
	assert.Nil(t, filtered)
}

func Test_FilterResponseByTime_SingleObjectBlockedAfterCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{"id": 1, "created_at": "2024-06-20T00:00:00Z"}`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(result, cutoff, config)
	require.NotNil(t, filtered)
	require.True(t, filtered.IsError)
	errorContent := getErrorResult(t, filtered)
	assert.Contains(t, errorContent.Text, "does not exist at the specified time travel cutoff")
}

func Test_FilterResponseByTime_StateMaskingNullsMergedAt(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{
		"id": 1,
		"created_at": "2024-06-10T00:00:00Z",
		"merged_at": "2024-06-20T00:00:00Z",
		"merge_commit_sha": "abc123",
		"state": "closed"
	}`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{
		TimestampKey: "created_at",
		StateFields: []StateMaskField{
			{
				TimestampKey:  "merged_at",
				NullFields:    []string{"merge_commit_sha"},
				StateOverride: "open",
			},
		},
	}

	filtered := FilterResponseByTime(result, cutoff, config)
	require.NotNil(t, filtered)
	require.False(t, filtered.IsError)

	textContent := getTextResult(t, filtered)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &obj))
	assert.Nil(t, obj["merged_at"])
	assert.Nil(t, obj["merge_commit_sha"])
	assert.Equal(t, "open", obj["state"])
}

func Test_FilterResponseByTime_StateMaskingPreservesWhenBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{
		"id": 1,
		"created_at": "2024-06-01T00:00:00Z",
		"merged_at": "2024-06-10T00:00:00Z",
		"merge_commit_sha": "abc123",
		"state": "closed"
	}`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{
		TimestampKey: "created_at",
		StateFields: []StateMaskField{
			{
				TimestampKey:  "merged_at",
				NullFields:    []string{"merge_commit_sha"},
				StateOverride: "open",
			},
		},
	}

	filtered := FilterResponseByTime(result, cutoff, config)
	// State masking should still be applied (returns non-nil) but fields preserved
	if filtered != nil {
		textContent := getTextResult(t, filtered)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(textContent.Text), &obj))
		assert.Equal(t, "2024-06-10T00:00:00Z", obj["merged_at"])
		assert.Equal(t, "abc123", obj["merge_commit_sha"])
		assert.Equal(t, "closed", obj["state"])
	}
}

func Test_FilterResponseByTime_MissingTimestampFieldPassesThrough(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `[
		{"id": 1, "name": "no-timestamp"},
		{"id": 2, "created_at": "2024-06-20T00:00:00Z"}
	]`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(result, cutoff, config)
	require.NotNil(t, filtered)
	require.False(t, filtered.IsError)

	textContent := getTextResult(t, filtered)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &items))
	// Item without timestamp passes through, item after cutoff is filtered
	assert.Len(t, items, 1)
	assert.Equal(t, float64(1), items[0]["id"])
}

func Test_FilterResponseByTime_EmptyArrayReturnsEmptyArray(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `[]`
	result := utils.NewToolResultText(input)
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(result, cutoff, config)
	require.NotNil(t, filtered)
	require.False(t, filtered.IsError)

	textContent := getTextResult(t, filtered)
	var items []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &items))
	assert.Empty(t, items)
}

// --- CheckExistence tests ---

func Test_CheckExistence_PassesWhenBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{"id": 1, "created_at": "2024-06-10T00:00:00Z"}`
	result := utils.NewToolResultText(input)

	checked := CheckExistence(result, cutoff, "created_at")
	assert.Nil(t, checked, "should return nil (pass-through) when item exists before cutoff")
}

func Test_CheckExistence_BlocksWhenAfterCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	input := `{"id": 1, "created_at": "2024-06-20T00:00:00Z"}`
	result := utils.NewToolResultText(input)

	checked := CheckExistence(result, cutoff, "created_at")
	require.NotNil(t, checked)
	require.True(t, checked.IsError)
	errorContent := getErrorResult(t, checked)
	assert.Contains(t, errorContent.Text, "does not exist")
}

// --- State masking tests ---

func Test_StateMasking_PRMergedAfterCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	obj := map[string]json.RawMessage{
		"state":            json.RawMessage(`"closed"`),
		"merged_at":        json.RawMessage(`"2024-06-20T00:00:00Z"`),
		"merge_commit_sha": json.RawMessage(`"abc123"`),
	}

	fields := []StateMaskField{
		{
			TimestampKey:  "merged_at",
			NullFields:    []string{"merge_commit_sha"},
			StateOverride: "open",
		},
	}

	result := applyStateMasking(obj, cutoff, fields)

	assert.Equal(t, json.RawMessage("null"), result["merged_at"])
	assert.Equal(t, json.RawMessage("null"), result["merge_commit_sha"])

	var state string
	require.NoError(t, json.Unmarshal(result["state"], &state))
	assert.Equal(t, "open", state)
}

func Test_StateMasking_PRMergedBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	obj := map[string]json.RawMessage{
		"state":            json.RawMessage(`"closed"`),
		"merged_at":        json.RawMessage(`"2024-06-10T00:00:00Z"`),
		"merge_commit_sha": json.RawMessage(`"abc123"`),
	}

	fields := []StateMaskField{
		{
			TimestampKey:  "merged_at",
			NullFields:    []string{"merge_commit_sha"},
			StateOverride: "open",
		},
	}

	result := applyStateMasking(obj, cutoff, fields)

	var mergedAt string
	require.NoError(t, json.Unmarshal(result["merged_at"], &mergedAt))
	assert.Equal(t, "2024-06-10T00:00:00Z", mergedAt)

	var sha string
	require.NoError(t, json.Unmarshal(result["merge_commit_sha"], &sha))
	assert.Equal(t, "abc123", sha)

	var state string
	require.NoError(t, json.Unmarshal(result["state"], &state))
	assert.Equal(t, "closed", state)
}

func Test_StateMasking_IssueClosedAfterCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	obj := map[string]json.RawMessage{
		"state":     json.RawMessage(`"closed"`),
		"closed_at": json.RawMessage(`"2024-06-20T00:00:00Z"`),
	}

	fields := []StateMaskField{
		{
			TimestampKey:  "closed_at",
			StateOverride: "open",
		},
	}

	result := applyStateMasking(obj, cutoff, fields)

	assert.Equal(t, json.RawMessage("null"), result["closed_at"])

	var state string
	require.NoError(t, json.Unmarshal(result["state"], &state))
	assert.Equal(t, "open", state)
}

func Test_StateMasking_IssueClosedBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	obj := map[string]json.RawMessage{
		"state":     json.RawMessage(`"closed"`),
		"closed_at": json.RawMessage(`"2024-06-10T00:00:00Z"`),
	}

	fields := []StateMaskField{
		{
			TimestampKey:  "closed_at",
			StateOverride: "open",
		},
	}

	result := applyStateMasking(obj, cutoff, fields)

	var closedAt string
	require.NoError(t, json.Unmarshal(result["closed_at"], &closedAt))
	assert.Equal(t, "2024-06-10T00:00:00Z", closedAt)

	var state string
	require.NoError(t, json.Unmarshal(result["state"], &state))
	assert.Equal(t, "closed", state)
}

// --- Edge case tests ---

func Test_CutoffChangeMidSession_ClearsCacheAndResolvesNewSHA(t *testing.T) {
	firstCutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	secondCutoff := time.Date(2024, 7, 1, 10, 0, 0, 0, time.UTC)
	callCount := 0

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: func(w http.ResponseWriter, r *http.Request) {
			callCount++
			sha := fmt.Sprintf("sha-call-%d", callCount)
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal([]*github.RepositoryCommit{
				{SHA: github.Ptr(sha)},
			})
			_, _ = w.Write(body)
		},
	})

	client := github.NewClient(mockedClient)
	state := NewTimeMaskingState()

	// First cutoff
	state.SetCutoff(&firstCutoff)
	sha1, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo")
	require.NoError(t, err)
	assert.Equal(t, "sha-call-1", sha1)

	// Change cutoff (should clear cache)
	state.SetCutoff(&secondCutoff)
	sha2, err := state.GetOrResolveSHA(context.Background(), client, "owner", "repo")
	require.NoError(t, err)
	assert.Equal(t, "sha-call-2", sha2)
	assert.Equal(t, 2, callCount, "should have made two API calls after cutoff change")
}

func Test_FilterResponseByTime_NilResult(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(nil, cutoff, config)
	assert.Nil(t, filtered)
}

func Test_FilterResponseByTime_ErrorResult(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)
	result := utils.NewToolResultError("some error")
	config := TimeFilterConfig{TimestampKey: "created_at"}

	filtered := FilterResponseByTime(result, cutoff, config)
	assert.Nil(t, filtered, "error results should pass through without filtering")
}

func Test_ConcurrentAccess(t *testing.T) {
	state := NewTimeMaskingState()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			cutoff := time.Date(2024, 6, i%28+1, 0, 0, 0, 0, time.UTC)
			state.SetCutoff(&cutoff)
		}(i)
		go func() {
			defer wg.Done()
			_ = state.GetCutoff()
		}()
	}
	wg.Wait()
	// No race condition = test passes (run with -race flag)
}

// --- Handler integration tests (TDD: these test expected behavior after handler modifications) ---

func Test_GetFileContents_TimeTravelOverridesSHA(t *testing.T) {
	// When time travel is active and no sha is provided, the handler should
	// resolve the historical SHA and use it for the content fetch.
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	historicalSHA := "historical123"

	mockContent := &github.RepositoryContent{
		Name:     github.Ptr("README.md"),
		Path:     github.Ptr("README.md"),
		SHA:      github.Ptr("fileSHA"),
		Type:     github.Ptr("file"),
		Content:  github.Ptr("dGVzdCBjb250ZW50"), // base64 "test content"
		Size:     github.Ptr(12),
		Encoding: github.Ptr("base64"),
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		// SHA resolver: returns historical commit
		GetReposCommitsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.RepositoryCommit{
			{SHA: github.Ptr(historicalSHA)},
		}),
		// The contents endpoint should be called with the historical SHA as ref
		GetReposContentsByOwnerByRepoByPath: func(w http.ResponseWriter, r *http.Request) {
			// Verify the ref query param uses the historical SHA
			ref := r.URL.Query().Get("ref")
			if ref != historicalSHA {
				t.Errorf("expected ref=%s, got ref=%s", historicalSHA, ref)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(mockContent)
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
		"path":  "README.md",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

func Test_GetFileContents_TimeTravelExplicitSHAPassesThrough(t *testing.T) {
	// When the caller provides an explicit sha, it should be used as-is
	// even when time travel is active.
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	callerSHA := "callerProvidedSHA"

	mockContent := &github.RepositoryContent{
		Name:     github.Ptr("README.md"),
		Path:     github.Ptr("README.md"),
		SHA:      github.Ptr("fileSHA"),
		Type:     github.Ptr("file"),
		Content:  github.Ptr("dGVzdCBjb250ZW50"),
		Size:     github.Ptr(12),
		Encoding: github.Ptr("base64"),
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposContentsByOwnerByRepoByPath: func(w http.ResponseWriter, r *http.Request) {
			ref := r.URL.Query().Get("ref")
			if ref != callerSHA {
				t.Errorf("expected ref=%s, got ref=%s", callerSHA, ref)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(mockContent)
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
		"path":  "README.md",
		"sha":   callerSHA,
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

func Test_GetFileContents_NoOverrideWhenTimeTravelInactive(t *testing.T) {
	// When time travel is not active, behavior should be unchanged.
	mockContent := &github.RepositoryContent{
		Name:     github.Ptr("README.md"),
		Path:     github.Ptr("README.md"),
		SHA:      github.Ptr("fileSHA"),
		Type:     github.Ptr("file"),
		Content:  github.Ptr("dGVzdCBjb250ZW50"),
		Size:     github.Ptr(12),
		Encoding: github.Ptr("base64"),
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposByOwnerByRepo:            mockResponse(t, http.StatusOK, `{"name": "repo", "default_branch": "main"}`),
		GetReposGitRefByOwnerByRepoByRef: mockResponse(t, http.StatusOK, `{"ref": "refs/heads/main", "object": {"sha": "defaultSHA"}}`),
		GetReposContentsByOwnerByRepoByPath: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(mockContent)
			_, _ = w.Write(body)
		},
	})

	// TimeMasking exists but no cutoff set
	state := NewTimeMaskingState()

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
		"path":  "README.md",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

func Test_GetFileContents_TimeTravelSHAResolverFailure(t *testing.T) {
	// If the SHA resolver fails, the handler should return an error.
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.RepositoryCommit{}),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
		"path":  "README.md",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.True(t, result.IsError)
	errorContent := getErrorResult(t, result)
	assert.Contains(t, errorContent.Text, "no commits found")
}

func Test_GetFileContents_TimeTravelRefOverriddenBySHA(t *testing.T) {
	// When time travel is active and caller provides ref but no sha,
	// the handler should override with the historical SHA.
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	historicalSHA := "historical456"

	mockContent := &github.RepositoryContent{
		Name:     github.Ptr("README.md"),
		Path:     github.Ptr("README.md"),
		SHA:      github.Ptr("fileSHA"),
		Type:     github.Ptr("file"),
		Content:  github.Ptr("dGVzdCBjb250ZW50"),
		Size:     github.Ptr(12),
		Encoding: github.Ptr("base64"),
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.RepositoryCommit{
			{SHA: github.Ptr(historicalSHA)},
		}),
		GetReposContentsByOwnerByRepoByPath: func(w http.ResponseWriter, r *http.Request) {
			ref := r.URL.Query().Get("ref")
			if ref != historicalSHA {
				t.Errorf("expected ref=%s (historical SHA), got ref=%s", historicalSHA, ref)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(mockContent)
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetFileContents(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
		"path":  "README.md",
		"ref":   "refs/heads/feature-branch",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

// --- get_repository_tree override tests ---

func Test_GetRepositoryTree_TimeTravelOverridesTreeSHA(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	historicalSHA := "historicalCommitSHA"
	historicalTreeSHA := "historicalTreeSHA"

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		// SHA resolver
		GetReposCommitsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.RepositoryCommit{
			{SHA: github.Ptr(historicalSHA)},
		}),
		// Fetch the commit to get its tree SHA
		GetReposGitCommitsByOwnerByRepoByCommitSHA: mockResponse(t, http.StatusOK, &github.Commit{
			SHA: github.Ptr(historicalSHA),
			Tree: &github.Tree{
				SHA: github.Ptr(historicalTreeSHA),
			},
		}),
		// Tree endpoint should be called with the historical tree SHA
		GetReposGitTreesByOwnerByRepoByTree: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(&github.Tree{
				SHA: github.Ptr(historicalTreeSHA),
				Entries: []*github.TreeEntry{
					{
						Path: github.Ptr("README.md"),
						Type: github.Ptr("blob"),
						SHA:  github.Ptr("blobSHA"),
						Mode: github.Ptr("100644"),
					},
				},
				Truncated: github.Ptr(false),
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetRepositoryTree(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

func Test_GetRepositoryTree_TimeTravelExplicitTreeSHAPassesThrough(t *testing.T) {
	callerTreeSHA := "callerTreeSHA"

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposGitTreesByOwnerByRepoByTree: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(&github.Tree{
				SHA:       github.Ptr(callerTreeSHA),
				Entries:   []*github.TreeEntry{},
				Truncated: github.Ptr(false),
			})
			_, _ = w.Write(body)
		},
	})

	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetRepositoryTree(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner":    "owner",
		"repo":     "repo",
		"tree_sha": callerTreeSHA,
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

func Test_GetRepositoryTree_NoOverrideWhenTimeTravelInactive(t *testing.T) {
	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposByOwnerByRepo: mockResponse(t, http.StatusOK, `{"name": "repo", "default_branch": "main"}`),
		GetReposGitRefByOwnerByRepoByRef: mockResponse(t, http.StatusOK, `{"ref": "refs/heads/main", "object": {"sha": "defaultSHA"}}`),
		GetReposGitTreesByOwnerByRepoByTree: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(&github.Tree{
				SHA:       github.Ptr("treeSHA"),
				Entries:   []*github.TreeEntry{},
				Truncated: github.Ptr(false),
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetRepositoryTree(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "expected success, got error: %v", result)
}

// --- Server-side injection tests ---

func Test_ListCommits_TimeTravelSetsUntil(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: func(w http.ResponseWriter, r *http.Request) {
			until := r.URL.Query().Get("until")
			if until == "" {
				t.Error("expected 'until' query param to be set")
			} else {
				parsedUntil, err := time.Parse(time.RFC3339, until)
				if err != nil {
					t.Errorf("failed to parse 'until' param: %v", err)
				} else if !parsedUntil.Equal(cutoff) {
					t.Errorf("expected until=%s, got until=%s", cutoff.Format(time.RFC3339), until)
				}
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal([]*github.RepositoryCommit{
				{SHA: github.Ptr("abc123")},
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := ListCommits(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func Test_ListCommits_NoUntilWhenTimeTravelInactive(t *testing.T) {
	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposCommitsByOwnerByRepo: func(w http.ResponseWriter, r *http.Request) {
			until := r.URL.Query().Get("until")
			if until != "" {
				t.Errorf("expected no 'until' param, got: %s", until)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal([]*github.RepositoryCommit{
				{SHA: github.Ptr("abc123")},
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := ListCommits(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func Test_SearchIssues_TimeTravelAppendsCreatedFilter(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		"GET /search/issues": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query().Get("q")
			expectedSuffix := fmt.Sprintf("created:<%s", cutoff.Format(time.RFC3339))
			if !strings.Contains(q, expectedSuffix) {
				t.Errorf("expected query to contain %q, got: %s", expectedSuffix, q)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(&github.IssuesSearchResult{
				Total:  github.Ptr(0),
				Issues: []*github.Issue{},
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := SearchIssues(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"query": "bug",
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func Test_SearchPullRequests_TimeTravelAppendsCreatedFilter(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		"GET /search/issues": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query().Get("q")
			expectedSuffix := fmt.Sprintf("created:<%s", cutoff.Format(time.RFC3339))
			if !strings.Contains(q, expectedSuffix) {
				t.Errorf("expected query to contain %q, got: %s", expectedSuffix, q)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(&github.IssuesSearchResult{
				Total:  github.Ptr(0),
				Issues: []*github.Issue{},
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := SearchPullRequests(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"query": "fix",
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
}

func Test_SearchRepositories_TimeTravelAppendsCreatedFilter(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		"GET /search/repositories": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query().Get("q")
			expectedSuffix := fmt.Sprintf("created:<%s", cutoff.Format(time.RFC3339))
			if !strings.Contains(q, expectedSuffix) {
				t.Errorf("expected query to contain %q, got: %s", expectedSuffix, q)
			}
			w.WriteHeader(http.StatusOK)
			body, _ := json.Marshal(&github.RepositoriesSearchResult{
				Total:        github.Ptr(0),
				Repositories: []*github.Repository{},
			})
			_, _ = w.Write(body)
		},
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := SearchRepositories(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"query": "mcp",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// --- Client-side filtering integration tests ---

func Test_ListPullRequests_TimeTravelFilters(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposPullsByOwnerByRepo: mockResponse(t, http.StatusOK, []*github.PullRequest{
			{
				Number:    github.Ptr(1),
				Title:     github.Ptr("Before cutoff"),
				CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
				State:     github.Ptr("open"),
			},
			{
				Number:    github.Ptr(2),
				Title:     github.Ptr("After cutoff"),
				CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
				State:     github.Ptr("open"),
			},
		}),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := ListPullRequests(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var prs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &prs))
	assert.Len(t, prs, 1, "only the PR before cutoff should remain")
}

// --- Phase 4: State masking + compound tool filtering ---

func Test_PullRequestRead_Get_StateMasking_MergedAfterCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockPR := &github.PullRequest{
		Number:    github.Ptr(42),
		Title:     github.Ptr("Test PR"),
		State:     github.Ptr("closed"),
		Merged:    github.Ptr(true),
		MergedAt:  &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
		ClosedAt:  &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
		CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
		HTMLURL:   github.Ptr("https://github.com/owner/repo/pull/42"),
		Head:      &github.PullRequestBranch{SHA: github.Ptr("abc123"), Ref: github.Ptr("feature")},
		Base:      &github.PullRequestBranch{Ref: github.Ptr("main")},
		User:      &github.User{Login: github.Ptr("testuser")},
		MergeCommitSHA: github.Ptr("merge123"),
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, mockPR),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := PullRequestRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":     "get",
		"owner":      "owner",
		"repo":       "repo",
		"pullNumber": float64(42),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var pr map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &pr))

	assert.Equal(t, "open", pr["state"], "state should be rolled back to open")
	assert.Nil(t, pr["merged_at"], "merged_at should be nulled")
	assert.Equal(t, false, pr["merged"], "merged should be false")
}

func Test_PullRequestRead_Get_StateMasking_MergedBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockPR := &github.PullRequest{
		Number:    github.Ptr(42),
		Title:     github.Ptr("Test PR"),
		State:     github.Ptr("closed"),
		Merged:    github.Ptr(true),
		MergedAt:  &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
		ClosedAt:  &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
		CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)},
		HTMLURL:   github.Ptr("https://github.com/owner/repo/pull/42"),
		Head:      &github.PullRequestBranch{SHA: github.Ptr("abc123"), Ref: github.Ptr("feature")},
		Base:      &github.PullRequestBranch{Ref: github.Ptr("main")},
		User:      &github.User{Login: github.Ptr("testuser")},
		MergeCommitSHA: github.Ptr("merge123"),
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposPullsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, mockPR),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := PullRequestRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":     "get",
		"owner":      "owner",
		"repo":       "repo",
		"pullNumber": float64(42),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var pr map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &pr))

	assert.Equal(t, "closed", pr["state"], "state should remain closed")
	assert.NotNil(t, pr["merged_at"], "merged_at should be preserved")
	assert.Equal(t, true, pr["merged"], "merged should remain true")
}

func Test_IssueRead_Get_StateMasking_ClosedAfterCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockIssue := &github.Issue{
		Number:    github.Ptr(10),
		Title:     github.Ptr("Test Issue"),
		State:     github.Ptr("closed"),
		ClosedAt:  &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
		CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)},
		HTMLURL:   github.Ptr("https://github.com/owner/repo/issues/10"),
		User:      &github.User{Login: github.Ptr("testuser")},
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposIssuesByOwnerByRepoByIssueNumber: mockResponse(t, http.StatusOK, mockIssue),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := IssueRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":       "get",
		"owner":        "owner",
		"repo":         "repo",
		"issue_number": float64(10),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var issue map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &issue))

	assert.Equal(t, "open", issue["state"], "state should be rolled back to open")
	assert.Nil(t, issue["closed_at"], "closed_at should be nulled")
}

func Test_IssueRead_GetComments_TimeTravelFilters(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockComments := []*github.IssueComment{
		{
			ID:        github.Ptr(int64(1)),
			Body:      github.Ptr("Before cutoff"),
			CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
			User:      &github.User{Login: github.Ptr("user1")},
			HTMLURL:   github.Ptr("https://github.com/owner/repo/issues/10#issuecomment-1"),
		},
		{
			ID:        github.Ptr(int64(2)),
			Body:      github.Ptr("After cutoff"),
			CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
			User:      &github.User{Login: github.Ptr("user2")},
			HTMLURL:   github.Ptr("https://github.com/owner/repo/issues/10#issuecomment-2"),
		},
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposIssuesCommentsByOwnerByRepoByIssueNumber: mockResponse(t, http.StatusOK, mockComments),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := IssueRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":       "get_comments",
		"owner":        "owner",
		"repo":         "repo",
		"issue_number": float64(10),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var comments []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &comments))
	assert.Len(t, comments, 1, "only the comment before cutoff should remain")
	assert.Equal(t, "Before cutoff", comments[0]["body"])
}

func Test_PullRequestRead_GetReviews_TimeTravelFilters(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockReviews := []*github.PullRequestReview{
		{
			ID:          github.Ptr(int64(201)),
			State:       github.Ptr("APPROVED"),
			Body:        github.Ptr("LGTM"),
			HTMLURL:     github.Ptr("https://github.com/owner/repo/pull/42#pullrequestreview-201"),
			User:        &github.User{Login: github.Ptr("reviewer1")},
			SubmittedAt: &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
		},
		{
			ID:          github.Ptr(int64(202)),
			State:       github.Ptr("CHANGES_REQUESTED"),
			Body:        github.Ptr("Needs work"),
			HTMLURL:     github.Ptr("https://github.com/owner/repo/pull/42#pullrequestreview-202"),
			User:        &github.User{Login: github.Ptr("reviewer2")},
			SubmittedAt: &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
		},
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposPullsReviewsByOwnerByRepoByPullNumber: mockResponse(t, http.StatusOK, mockReviews),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := PullRequestRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":     "get_reviews",
		"owner":      "owner",
		"repo":       "repo",
		"pullNumber": float64(42),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var reviews []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &reviews))
	assert.Len(t, reviews, 1, "only the review before cutoff should remain")
	assert.Equal(t, "APPROVED", reviews[0]["state"])
}

func Test_PullRequestRead_GetComments_TimeTravelFilters(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockComments := []*github.IssueComment{
		{
			ID:        github.Ptr(int64(1)),
			Body:      github.Ptr("Old comment"),
			CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
			User:      &github.User{Login: github.Ptr("user1")},
			HTMLURL:   github.Ptr("https://github.com/owner/repo/pull/42#issuecomment-1"),
		},
		{
			ID:        github.Ptr(int64(2)),
			Body:      github.Ptr("New comment"),
			CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
			User:      &github.User{Login: github.Ptr("user2")},
			HTMLURL:   github.Ptr("https://github.com/owner/repo/pull/42#issuecomment-2"),
		},
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposIssuesCommentsByOwnerByRepoByIssueNumber: mockResponse(t, http.StatusOK, mockComments),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := PullRequestRead(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"method":     "get_comments",
		"owner":      "owner",
		"repo":       "repo",
		"pullNumber": float64(42),
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)

	textContent := getTextResult(t, result)
	var comments []map[string]any
	require.NoError(t, json.Unmarshal([]byte(textContent.Text), &comments))
	assert.Len(t, comments, 1, "only the comment before cutoff should remain")
	assert.Equal(t, "Old comment", comments[0]["body"])
}

func Test_GetLatestRelease_TimeTravelExistenceCheck(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockRelease := &github.RepositoryRelease{
		ID:        github.Ptr(int64(1)),
		TagName:   github.Ptr("v2.0.0"),
		Name:      github.Ptr("Release 2.0"),
		CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 20, 0, 0, 0, 0, time.UTC)},
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposReleasesLatestByOwnerByRepo: mockResponse(t, http.StatusOK, mockRelease),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetLatestRelease(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.True(t, result.IsError, "should return error when release is after cutoff")
	errorContent := getErrorResult(t, result)
	assert.Contains(t, errorContent.Text, "does not exist")
}

func Test_GetLatestRelease_TimeTravelBeforeCutoff(t *testing.T) {
	cutoff := time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC)

	mockRelease := &github.RepositoryRelease{
		ID:        github.Ptr(int64(1)),
		TagName:   github.Ptr("v1.0.0"),
		Name:      github.Ptr("Release 1.0"),
		CreatedAt: &github.Timestamp{Time: time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC)},
	}

	mockedClient := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposReleasesLatestByOwnerByRepo: mockResponse(t, http.StatusOK, mockRelease),
	})

	state := NewTimeMaskingState()
	state.SetCutoff(&cutoff)

	client := github.NewClient(mockedClient)
	deps := BaseDeps{
		Client:      client,
		TimeMasking: state,
	}

	serverTool := GetLatestRelease(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner",
		"repo":  "repo",
	})

	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError, "release before cutoff should be returned normally")
}
