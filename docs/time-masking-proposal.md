# Proposal: Time-Masking Mode for Incident Investigation Regression Testing

## Problem

We need to regression-test an incident investigation agent against past incidents. The agent uses this GitHub MCP server to gather information. For accurate regression testing, the agent must only see data that existed at the time of the incident -- it must not see commits, PRs, issues, alerts, or code changes that happened after the incident cutoff time.

## Design: Runtime Time-Travel via Tool Call

The agent (or test harness) calls a `set_time_travel` tool at runtime to activate time-masking. This sets a cutoff timestamp that transparently filters all subsequent tool responses. The cutoff can be updated mid-session to simulate time elapsing during the incident.

All existing tools keep their same interface and contract. The filtering is internal and invisible to the caller.

### `set_time_travel` tool

```
Tool: set_time_travel
Params:
  cutoff: string (ISO 8601) - timestamp to travel to. Empty string disables time travel.
Returns:
  Confirmation with the active cutoff timestamp.
```

When called:
1. Parses and stores the cutoff timestamp in session state
2. Clears all caches (SHA resolver, etc.) since new cutoff means different historical state
3. Blocks all write tools (enforces read-only mode)
4. Returns confirmation

### Why a tool call, not a CLI flag

- The cutoff can be updated mid-session (simulating time progressing during an incident)
- The same server instance can serve multiple test scenarios without restart
- The agent or test harness explicitly controls the time context

## Architecture

```
┌──────────────────────────────────────────────────┐
│  set_time_travel tool                            │
│  Sets/clears cutoff on shared session state      │
└──────────────┬───────────────────────────────────┘
               │
┌──────────────▼───────────────────────────────────┐
│  TimeMaskingState (on BaseDeps)                  │
│  sync.RWMutex-protected                          │
│  + cutoff *time.Time                             │
│  + shaCache map[owner/repo] → commit SHA         │
│                                                  │
│  Visible to all tool handlers via deps           │
└──────────────┬───────────────────────────────────┘
               │
               │  Each tool handler checks:
               │  1. Is time travel active?
               │  2. Am I a write tool? → block
               │  3. Do I need SHA override? → resolve
               │  4. Do I need response filtering? → filter
               │
┌──────────────▼───────────────────────────────────┐
│  SHAResolver (lazy, cached per repo)             │
│  (owner, repo) → HEAD commit SHA at cutoff       │
│  Uses: list_commits(until=cutoff, per_page=1)    │
└──────────────────────────────────────────────────┘
```

### Session state in Go

`BaseDeps` (dependencies.go:102) is created once at startup and shared across all tool calls via context injection. A mutex-protected field on it is visible to all subsequent handlers:

```go
type TimeMaskingState struct {
    mu       sync.RWMutex
    cutoff   *time.Time
    shaCache map[string]string // "owner/repo" -> SHA at cutoff
}
```

Added as a field on `BaseDeps`. For stdio (single connection, sequential requests) the mutex is technically unnecessary but costs nothing and makes it safe for HTTP mode too.

### New files

| File | Purpose |
|---|---|
| `pkg/github/time_masking.go` | TimeMaskingState, set_time_travel tool, SHA resolver, response filters |
| `pkg/github/time_masking_test.go` | Tests |

### Modified files

| File | Change |
|---|---|
| `pkg/github/dependencies.go` | Add `TimeMasking *TimeMaskingState` field to `BaseDeps` |
| `pkg/github/server.go` | Register `set_time_travel` tool, wire state into deps |

## Tool-by-Tool Strategy

### Tier 1: Code Access Tools (SHA Override)

These tools need the ref/sha parameter overridden to the historical commit at the cutoff time.

| Tool | Current params | Strategy |
|---|---|---|
| `get_file_contents` | ref, sha | Override `sha` with resolved historical SHA. If caller provides a ref, resolve it relative to cutoff. |
| `get_repository_tree` | tree_sha | Override `tree_sha` with resolved historical SHA's tree. |
| `search_code` | query | **No change.** Searches current index. Agent gets file paths, then reads historical content via `get_file_contents` (which uses SHA override). Files added after cutoff will 404 on read -- agent moves on naturally. Deleted files are a known false-negative gap. |

**SHA Resolution**: On first code-access call for a given (owner, repo), call `Repositories.ListCommits(until=cutoff, per_page=1)` to get the HEAD SHA at that moment. Cache in `shaCache`. Cleared when cutoff changes.

### Tier 2: List Tools with Server-Side Time Filtering

These tools can use existing API parameters to exclude future data. The handler injects the filter transparently.

| Tool | API support | Injection strategy |
|---|---|---|
| `list_commits` | `CommitsListOptions.Until` (go-github has it, MCP doesn't expose it) | Set `Until = cutoff`. ~3 lines in handler. |
| `list_notifications` | `NotificationListOptions.Before` | Set `Before = cutoff` if not already set by caller. |
| `search_issues` | Query syntax: `created:<date` | Append `created:<{cutoff_iso}` to query string. |
| `search_pull_requests` | Query syntax: `created:<date` | Append `created:<{cutoff_iso}` to query string. |
| `search_repositories` | Query syntax: `created:<date` | Append `created:<{cutoff_iso}` to query string. |
| `actions_list` (workflow runs) | `ListWorkflowRunsOptions.Created` (string, supports range syntax `<date`) | Set `Created = "<{cutoff_iso}"`. ~3 lines. |
| `list_global_security_advisories` | `published`, `updated`, `modified` params | Set `published = "<{cutoff_iso}"` to exclude future advisories. |

### Tier 3: List Tools Requiring Client-Side Filtering

These return timestamped data but the API has no date filter. The handler post-processes the response to remove items where `created_at > cutoff`.

| Tool | Timestamp field to filter on | Notes |
|---|---|---|
| `list_pull_requests` | `created_at` | API has no date params. |
| `list_releases` | `created_at` or `published_at` | API has no date filter. |
| `list_issues` | `created_at` | GraphQL `since` is a lower bound, not upper. Need client-side `created_at <= cutoff`. |
| `list_code_scanning_alerts` | `created_at` | |
| `list_secret_scanning_alerts` | `created_at` | |
| `list_dependabot_alerts` | `created_at` | |
| `list_discussions` | `createdAt` | GraphQL response. |
| `list_gists` | `created_at` | `since` is lower-bound only. |
| `projects_list` | `createdAt` | GraphQL response. |
| `list_repository_security_advisories` | `published_at` | `before` param is a cursor, not a date. |
| `list_org_repository_security_advisories` | `published_at` | Same as above. |
| `list_starred_repositories` | `starred_at` | |
| `list_branches` | None | See note below. |
| `list_tags` | None | See note below. |

**Note on branches/tags**: These have no timestamps. Branches and tags created after the cutoff cannot be filtered. Known limitation for v1. Could be mitigated by checking each branch HEAD's commit date against the cutoff, at the cost of an extra API call per branch.

### Tier 4: Single-Item Fetches (Existence Check)

When the agent fetches a specific issue, PR, alert, etc. by ID/number, the handler checks `created_at <= cutoff` and returns a 404-style error if the item didn't exist yet.

| Tool | Timestamp to check |
|---|---|
| `issue_read` (method: get) | `created_at` |
| `issue_read` (method: get_comments) | Filter individual comments by `created_at` |
| `issue_read` (method: get_sub_issues) | Filter by `created_at` |
| `pull_request_read` (method: get) | `created_at` |
| `pull_request_read` (method: get_diff) | Use historical SHA for diff |
| `pull_request_read` (method: get_reviews) | Filter reviews by `submitted_at` |
| `pull_request_read` (method: get_comments) | Filter comments by `created_at` |
| `pull_request_read` (method: get_review_comments) | Filter by `createdAt` |
| `pull_request_read` (method: get_check_runs) | Filter by `started_at` |
| `get_commit` | `commit.committer.date` |
| `get_gist` | `created_at` |
| `get_code_scanning_alert` | `created_at` |
| `get_secret_scanning_alert` | `created_at` |
| `get_dependabot_alert` | `created_at` |
| `get_discussion` | `createdAt` |
| `get_discussion_comments` | Filter by `createdAt` |
| `get_notification_details` | `updated_at` |
| `get_global_security_advisory` | `published_at` |
| `get_latest_release` | `created_at` (must find latest release that existed at cutoff) |
| `get_release_by_tag` | `created_at` |

**Subtlety for comments/reviews**: The agent may fetch an issue that existed before the cutoff, but some comments were added after. The handler filters the comments list, not the entire issue.

**Subtlety for PR state**: A PR that existed before the cutoff may have been merged after it. The handler masks the merged state: set `merged_at = null`, `state = "open"`, `merge_commit_sha = null` if `merged_at > cutoff`. Same logic for issues closed after cutoff.

### Tier 5: Write Tools (Blocked)

All tools with `ReadOnlyHint: false` return an error when time travel is active. Checked via `TimeMaskingState.GetCutoff() != nil` at the start of each write handler.

Affected tools (26 total):
- `create_or_update_file`, `create_repository`, `fork_repository`, `delete_file`, `create_branch`, `push_files`
- `issue_write`, `add_issue_comment`, `sub_issue_write`
- `create_pull_request`, `update_pull_request`, `merge_pull_request`, `update_pull_request_branch`, `add_reply_to_pull_request_comment`, `pull_request_review_write`, `add_comment_to_pending_review`
- `assign_copilot_to_issue`, `request_copilot_review`
- `create_gist`, `update_gist`
- `dismiss_notification`, `mark_all_notifications_read`, `manage_notification_subscription`, `manage_repository_notification_subscription`
- `label_write`, `projects_write`
- `actions_run_trigger`

### Tier 6: Unchanged / No Time Dimension

These tools are not meaningfully time-dependent:
- `get_me`, `get_teams`, `get_team_members` (org structure; assume stable)
- `search_users`, `search_orgs` (user/org existence; assume stable)
- `get_label`, `list_label` (labels have no timestamps)
- `list_issue_types` (schema, not data)
- `list_discussion_categories` (schema)
- `enable_toolset`, `list_available_toolsets`, `get_toolset_tools` (internal)
- `get_job_logs` (content of a specific job; if the job existed, the logs are fine)

## Implementation Status

Infrastructure is already in place (`pkg/github/time_masking.go`, `pkg/github/dependencies.go` changes). TDD tests written in `pkg/github/time_masking_test.go`. Implementation proceeds in four phases below.

## Implementation Plan

### Phase 1: SHA Override (get_file_contents, get_repository_tree)

**Goal**: Agent sees correct historical code when time travel is active.

**Files modified**:
- `pkg/github/repositories.go` -- `get_file_contents` handler
- `pkg/github/git.go` -- `get_repository_tree` handler

**Changes**:

1. `get_file_contents` (repositories.go): at the start of the handler, after extracting `owner`, `repo`, `ref`, `sha` params:
   - Check `deps.GetTimeMasking()` for an active cutoff
   - If active and caller did NOT provide a `sha` param:
     - Call `state.GetOrResolveSHA(ctx, client, owner, repo)` to get historical commit SHA
     - Set `sha = historicalSHA` (this feeds into `resolveGitReference` which gives SHA highest priority)
   - If caller provides explicit `sha`: use as-is (trust it)
   - If SHA resolution fails: return tool error

2. `get_repository_tree` (git.go): at the start of the handler, after extracting `owner`, `repo`, `tree_sha` params:
   - Check `deps.GetTimeMasking()` for an active cutoff
   - If active and caller did NOT provide a `tree_sha` param:
     - Call `state.GetOrResolveSHA(ctx, client, owner, repo)` to get historical commit SHA
     - Fetch that commit via `client.Git.GetCommit()` to extract its tree SHA
     - Set `tree_sha = commit.Tree.SHA`
   - If caller provides explicit `tree_sha`: use as-is

**TDD tests that should pass after this phase**:
- `Test_GetFileContents_TimeTravelOverridesSHA`
- `Test_GetFileContents_TimeTravelExplicitSHAPassesThrough` (already passes)
- `Test_GetFileContents_NoOverrideWhenTimeTravelInactive` (already passes)
- `Test_GetFileContents_TimeTravelSHAResolverFailure`
- `Test_GetFileContents_TimeTravelRefOverriddenBySHA`
- `Test_GetRepositoryTree_TimeTravelOverridesTreeSHA`
- `Test_GetRepositoryTree_TimeTravelExplicitTreeSHAPassesThrough` (already passes)
- `Test_GetRepositoryTree_NoOverrideWhenTimeTravelInactive` (already passes)

### Phase 2: Server-Side Injection (list_commits, search tools)

**Goal**: List and search tools use API-level time filtering to exclude future data.

**Files modified**:
- `pkg/github/repositories.go` -- `list_commits` handler
- `pkg/github/search_utils.go` -- `searchHandler` (shared by `search_issues`, `search_pull_requests`)
- `pkg/github/search.go` -- `SearchRepositories` handler

**Changes** (~3-5 lines per handler):

1. `list_commits` (repositories.go): after constructing `CommitsListOptions`, before the API call:
   - If time travel active: `opts.Until = *cutoff`

2. `search_issues` / `search_pull_requests` (search_utils.go): in the shared `searchHandler`, before the API call:
   - If time travel active: append ` created:<{cutoff_iso}` to the query string

3. `search_repositories` (search.go): before the API call:
   - If time travel active: append ` created:<{cutoff_iso}` to the query string

**TDD tests that should pass after this phase**:
- `Test_ListCommits_TimeTravelSetsUntil`
- `Test_ListCommits_NoUntilWhenTimeTravelInactive` (already passes)
- `Test_SearchIssues_TimeTravelAppendsCreatedFilter`
- `Test_SearchPullRequests_TimeTravelAppendsCreatedFilter`
- `Test_SearchRepositories_TimeTravelAppendsCreatedFilter`

### Phase 3: Client-Side Filtering + Write Blocking in NewTool Wrapper

**Goal**: Tools with no API-level date filter get client-side post-filtering. Write tools are blocked. `set_time_travel` tool is registered and usable.

**Files modified**:
- `pkg/github/dependencies.go` -- `NewTool` and `NewToolFromHandler` wrappers
- `pkg/github/server.go` -- register `set_time_travel` tool

**Changes**:

1. `NewTool` wrapper (dependencies.go): wrap the handler invocation:
   - **Pre-handler**: if time travel active AND `tool.Annotations.ReadOnlyHint` is false/nil, return error ("write operations are blocked during time travel")
   - **Post-handler**: if time travel active AND `toolTimeFilters[tool.Name]` exists, call `FilterResponseByTime` on the result. If it returns non-nil, use the filtered result.
   - Same logic in `NewToolFromHandler`

2. `server.go`: register `set_time_travel` tool directly (not via inventory/toolsets):
   - Create `TimeMaskingState` in `NewMCPServer`
   - Register `SetTimeTravel` tool with `SetTimeTravelHandler`
   - Pass state to deps

The `toolTimeFilters` map and `FilterResponseByTime` function already exist in `time_masking.go`.

**TDD tests that should pass after this phase**:
- `Test_ListPullRequests_TimeTravelFilters`

**Existing unit tests already passing that validate this logic**:
- All `Test_IsWriteBlocked_*` tests
- All `Test_FilterResponseByTime_*` tests

### Phase 4: State Masking + Edge Cases

**Goal**: Individual item state is historically accurate. Edge cases handled.

**Files modified**:
- `pkg/github/time_masking.go` -- add state masking configs to `toolTimeFilters`
- Per-handler changes for compound tools (issue_read, pull_request_read)
- `pkg/github/repositories.go` -- `get_latest_release` special handling

**Changes**:

1. Add state masking to `toolTimeFilters` for PR and issue single-item fetches:
   - PR: `merged_at` > cutoff -> null `merged_at`, `merge_commit_sha`, set `state = "open"`
   - Issue: `closed_at` > cutoff -> null `closed_at`, set `state = "open"`

2. Comment/review sub-filtering for compound tools:
   - `issue_read` (get_comments): filter comments array by `created_at`
   - `pull_request_read` (get_reviews): filter by `submitted_at`
   - `pull_request_read` (get_comments, get_review_comments): filter by `created_at`/`createdAt`
   - `pull_request_read` (get_check_runs): filter by `started_at`
   - `get_discussion_comments`: filter by `createdAt`
   - These are per-handler changes since the compound tools have method-specific logic

3. `get_latest_release`: fetch `list_releases`, filter by `created_at <= cutoff`, return first result.

4. Existence checks for single-item fetches (via `toolExistenceChecks` map, already defined in `time_masking.go`):
   - `get_commit`, `get_gist`, alert endpoints, `get_release_by_tag`

**Existing unit tests already passing that validate the filtering logic**:
- All `Test_StateMasking_*` tests
- All `Test_CheckExistence_*` tests

## Centralized Hook Architecture

The `NewTool` wrapper in `dependencies.go` is the central integration point (wired in Phase 3). The flow becomes:

```
NewTool handler wrapper:
  1. Extract deps from context
  2. PRE-HANDLER: If time travel active AND tool is write → return error
  3. Call original handler
  4. POST-HANDLER: If time travel active AND tool has filter config → apply filter
  5. Return result
```

This keeps individual handlers mostly unchanged. Only Phase 1 (SHA override) and Phase 2 (server-side injection) require per-handler modifications.

## Known Limitations

1. **Code Search**: GitHub Code Search indexes only the current default branch. `search_code` returns results against current content. Files added after the cutoff will 404 when the agent tries to read them via `get_file_contents`. Files deleted since the cutoff won't appear in search results (false negatives). For most incident investigations this is acceptable since the codebase at the cutoff is usually very close to the current state.

2. **Branch/tag listings**: No timestamps on branches or tags. Lists will include entries created after the cutoff. The SHA resolver mitigates this for code access (the agent gets the right code regardless), but the listings themselves are unfiltered.

3. **Issue/PR body edits**: If an issue body was edited after the cutoff, we return the current (latest) body, not the body as it existed at the cutoff. Reconstructing historical body text would require the issue timeline API.

4. **Label/milestone changes**: Labels applied to an issue after the cutoff will still appear. The timeline API could reconstruct historical labels but this is expensive.

5. **Pagination with client-side filtering**: When filtering items client-side, a page of 30 items might be reduced to 15 after filtering. The caller may need to paginate more to get a full page.

6. **Rate limiting**: The SHA resolver adds one extra API call per unique (owner, repo) pair. Client-side filtering doesn't add API calls but may waste some quota on items that get filtered out.

## Estimated Scope

| Phase | Description | Files | Failing TDD Tests Resolved | Value |
|---|---|---|---|---|
| Phase 1 | SHA override for code access tools | repositories.go, git.go | 3 | Agent sees correct historical code |
| Phase 2 | Server-side time injection for list/search tools | repositories.go, search_utils.go, search.go | 3 | Lists/searches exclude future data |
| Phase 3 | Client-side filtering + write blocking in NewTool wrapper | dependencies.go, server.go | 1 | Generic filtering for all list tools, write blocking, tool registration |
| Phase 4 | State masking + edge cases | time_masking.go, per-handler compound tools | 0 (new tests needed) | Historically accurate item state |

All changes are in `pkg/github/`. No external dependencies added. Fully backwards-compatible (no behavior change when `set_time_travel` is not called).

Infrastructure already implemented:
- `pkg/github/time_masking.go` -- `TimeMaskingState`, `SetTimeTravel` tool, `FilterResponseByTime`, `CheckExistence`, `IsWriteBlocked`, filter config maps
- `pkg/github/dependencies.go` -- `TimeMasking` field on `BaseDeps`, `GetTimeMasking()` on `ToolDependencies` interface
- `pkg/github/time_masking_test.go` -- 48 tests (30 passing, 11 failing TDD handler tests, 7 already-passing no-op/backwards-compat tests)

## Test Plan

Tests follow existing codebase conventions: table-driven tests, `MockHTTPClientWithHandlers` for HTTP mocking, `toolsnaps.Test` for schema snapshots, `BaseDeps` struct for dependency injection, `serverTool.Handler(deps)` for invoking handlers, `createMCPRequest` for building requests, and `getTextResult`/`getErrorResult` for asserting results. All tests go in `pkg/github/time_masking_test.go` unless they modify an existing tool's test file.

### 1. TimeMaskingState Unit Tests (~6 tests)

Test the state struct in isolation:
- `SetCutoff` stores the cutoff and `GetCutoff` returns it
- `SetCutoff(nil)` disables time travel, `GetCutoff` returns nil
- `SetCutoff` clears the SHA cache (set a cache entry, change cutoff, verify cache is empty)
- `GetOrResolveSHA` calls ListCommits with correct `Until` and `PerPage=1` params
- `GetOrResolveSHA` caches result (second call does NOT hit the API)
- `GetOrResolveSHA` returns error when ListCommits fails (e.g., repo not found)

### 2. set_time_travel Tool Tests (~5 tests)

Test the tool handler directly:
- Valid ISO 8601 cutoff string sets the cutoff and returns confirmation
- Empty string disables time travel, returns "disabled" confirmation
- Invalid timestamp string returns error
- Calling twice updates the cutoff (verify old cutoff replaced)
- Verify schema snapshot (`toolsnaps.Test`)

### 3. Write Blocking Tests (~4 tests)

Test the `NewTool` wrapper's pre-handler check:
- Write tool (ReadOnlyHint=false) returns error when time travel is active
- Write tool succeeds when time travel is NOT active
- Read tool (ReadOnlyHint=true) succeeds when time travel is active
- `set_time_travel` itself succeeds when time travel is active (exempt from blocking)

### 4. SHA Resolver Tests (~4 tests)

Test `GetOrResolveSHA` with mocked HTTP:
- Returns the SHA from the first commit in the ListCommits response
- Passes `Until=cutoff` and `PerPage=1` as query params to the API
- Caches per (owner, repo) -- different repos get separate API calls
- Returns error for empty commit list (no commits before cutoff)

### 5. get_file_contents Override Tests (~5 tests)

Test the SHA override behavior in the handler:
- Time travel active, no ref/sha provided: request uses resolved historical SHA
- Time travel active, explicit `sha` provided: uses caller's SHA as-is
- Time travel active, `ref` provided but no `sha`: overrides with historical SHA
- Time travel inactive: no override, behaves normally
- SHA resolver failure propagates as tool error

### 6. get_repository_tree Override Tests (~3 tests)

- Time travel active, no tree_sha provided: resolves historical commit, fetches its tree SHA
- Time travel active, explicit tree_sha provided: uses as-is
- Time travel inactive: no override

### 7. Generic Response Filter Tests (~8 tests)

Test `FilterResponseByTime` in isolation with JSON strings:
- Filters top-level array: removes items with `created_at > cutoff`, keeps items with `created_at <= cutoff`
- Filters embedded array (ArrayKey="items"): removes items from the nested array
- Single object existence check: returns nil (pass-through) when `created_at <= cutoff`
- Single object existence check: returns error when `created_at > cutoff`
- State masking: nulls `merged_at` and sets `state="open"` when `merged_at > cutoff`
- State masking: leaves `merged_at` intact when `merged_at <= cutoff`
- Handles missing timestamp field gracefully (item passes through)
- Empty array input returns empty array (not an error)

### 8. State Masking Tests (~4 tests)

Test PR/issue state rollback:
- PR merged after cutoff: `merged_at` nulled, `merge_commit_sha` nulled, `state` set to "open"
- PR merged before cutoff: all fields preserved
- Issue closed after cutoff: `closed_at` nulled, `state` set to "open"
- Issue closed before cutoff: fields preserved

### 9. Server-Side Injection Tests (~7 tests)

Per-handler tests in respective test files (or `time_masking_test.go`):
- `list_commits`: verify `Until` query param is set when time travel active
- `list_commits`: verify `Until` is NOT set when time travel inactive
- `search_issues`: verify `created:<cutoff>` appended to query
- `search_pull_requests`: verify `created:<cutoff>` appended to query
- `search_repositories`: verify `created:<cutoff>` appended to query
- `actions_list`: verify `Created` param set to `<cutoff_iso>`
- `list_notifications`: verify `Before` param set when time travel active and not already set by caller

### 10. Client-Side Filtering Integration Tests (~6 tests)

End-to-end tests that invoke a tool handler with time travel active and verify the filtered result:
- `list_pull_requests` with mix of PRs before/after cutoff: only pre-cutoff PRs in result
- `list_releases` with mix: only pre-cutoff releases
- `list_code_scanning_alerts` with mix: only pre-cutoff alerts
- `list_issues` with mix: only pre-cutoff issues
- `issue_read` (get_comments) with comments spanning cutoff: only pre-cutoff comments
- `pull_request_read` (get_reviews) with reviews spanning cutoff: only pre-cutoff reviews

### 11. Edge Case Tests (~4 tests)

- `get_latest_release` with time travel: returns latest release before cutoff, not the actual latest
- Cutoff change mid-session: set cutoff A, resolve SHA, set cutoff B, verify new SHA resolved (cache cleared)
- Tool with no filter config: result passes through unmodified when time travel active
- Concurrent access: multiple goroutines reading cutoff while one writes (verify no race with `-race` flag)

### Test Total: ~56 tests

## Phase 5: Integration Tests

### Goal

Verify end-to-end time-travel behavior against the real GitHub API. Unit tests mock HTTP responses and validate filtering logic in isolation. Integration tests confirm that the filtering works correctly when actual GitHub API responses flow through the full tool handler pipeline.

### Approach

The existing e2e suite (`e2e/e2e_test.go`) uses Docker or in-process server setup with MCP client/server protocol. Time-travel integration tests should NOT use that approach for two reasons:

1. The e2e in-process mode is currently broken (`ghmcp.NewMCPServer` is undefined)
2. Time masking requires `BaseDeps.TimeMasking` to be set, which requires constructing deps directly -- not going through the MCP client protocol

Instead, integration tests will live in `pkg/github/time_masking_integration_test.go` behind a `//go:build integration` build tag. They construct `BaseDeps` directly with a real GitHub client and `TimeMaskingState`, then call tool handlers the same way unit tests do but against the live API.

### Prerequisites

- `GITHUB_TOKEN` environment variable with a PAT that has `repo` scope
- A test repository with known historical data (see "Test Fixture Repository" below)

### Test Fixture Repository

Tests require a repository with known data at known timestamps. Two approaches:

**Option A: Dedicated fixture repo (preferred)**

Create a public repo (e.g., `github-mcp-server-time-travel-fixtures`) with pre-populated data at known dates. Tests hardcode the repo name and expected timestamps. This avoids setup/teardown cost and rate limit usage.

Required fixture data:
- At least 2 commits: one before the test cutoff, one after
- An issue created before the cutoff, closed after the cutoff
- An issue comment posted before the cutoff, another after
- A PR created before the cutoff, merged after the cutoff
- A PR review submitted before the cutoff, another after
- A release created before the cutoff, another after (the "latest" release should be after the cutoff)

The cutoff timestamp is chosen between the "before" and "after" data points.

**Option B: Dynamic setup (fallback)**

Tests create a repo, populate it with data, record timestamps, then run assertions. This is slower (multiple API calls for setup) and more fragile (timing-sensitive) but self-contained.

Recommendation: Start with Option A. Create the fixture repo once manually, document its structure in a comment at the top of the test file.

### Test Cases

All tests follow the pattern:
1. Create `BaseDeps` with real GitHub client + `TimeMaskingState` with cutoff set
2. Call tool handler via `serverTool.Handler(deps)` + `ContextWithDeps`
3. Assert on the filtered response

#### 5.1 SHA Override Integration (~2 tests)

- **get_file_contents with time travel**: Set cutoff before the latest commit. Call `get_file_contents` without a ref. Verify the response contains the file content as it existed at the cutoff (not the current HEAD).
- **get_repository_tree with time travel**: Set cutoff before the latest commit. Call `get_repository_tree` without a tree_sha. Verify the tree matches the historical commit (e.g., a file added after the cutoff should not appear).

#### 5.2 List Filtering Integration (~3 tests)

- **list_commits with time travel**: Set cutoff. Call `list_commits`. Verify all returned commits have `committer.date <= cutoff`.
- **list_issues with time travel (TODO)**: Pending -- `list_issues` uses client-side filtering via the centralized hook. Verify issues created after cutoff are excluded. (Requires fixture issues.)
- **list_pull_requests with time travel (TODO)**: Same pattern as issues.

#### 5.3 Compound Tool Integration (~4 tests)

- **issue_read "get" state masking**: Fetch the issue that was closed after the cutoff. Verify `state == "open"` and `closed_at == null` in the response.
- **issue_read "get_comments" filtering**: Fetch comments for the issue. Verify only comments created before the cutoff are returned.
- **pull_request_read "get" state masking**: Fetch the PR that was merged after the cutoff. Verify `state == "open"`, `merged == false`, `merged_at == null`.
- **pull_request_read "get_reviews" filtering**: Fetch reviews for the PR. Verify only reviews submitted before the cutoff are returned.

#### 5.4 Existence Check Integration (~1 test)

- **get_latest_release with time travel**: Set cutoff before the latest release. Call `get_latest_release`. Verify it returns an error (the latest release didn't exist yet at the cutoff).

#### 5.5 Write Blocking Integration (~1 test)

- **Write tool blocked**: Set time travel active. Call a write tool (e.g., `issue_write` method "create"). Verify the response is an error containing "write operations are blocked".

#### 5.6 set_time_travel Round-Trip (~1 test)

- **Enable and disable**: Call `set_time_travel` with a cutoff. Verify a read tool returns filtered data. Call `set_time_travel` with empty string. Verify the same read tool returns unfiltered data.

### Test Total: ~12 integration tests

### Implementation Steps

1. Create the fixture repository on GitHub with the required data (manual, one-time)
2. Document the fixture repo structure and timestamps in a constants block at the top of the test file
3. Write `pkg/github/time_masking_integration_test.go` with `//go:build integration` tag
4. Add a helper function that creates `BaseDeps` with a real GitHub client from `GITHUB_TOKEN`
5. Implement test cases, starting with SHA override (most straightforward to verify)
6. Add a `make integration-test` target or document the run command: `go test -tags integration ./pkg/github/ -run TestTimeTravel -v`

### Considerations

- **Rate limiting**: Each test makes 1-3 API calls. The full suite uses ~20-30 API calls. Well within rate limits for a single run.
- **Flakiness**: Tests against a fixture repo with static data should be deterministic. No timing races.
- **CI**: Integration tests should run as a separate CI job, not as part of the default `go test`. They require a token and network access.
- **Fixture repo maintenance**: If the fixture repo is deleted or modified, tests break. Document this dependency clearly.
