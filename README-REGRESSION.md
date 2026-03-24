# Regression Testing with Time Masking

This fork adds a time-masking feature to the GitHub MCP Server. It allows an AI agent to view GitHub data as it existed at a specific point in time, enabling reproducible regression testing of incident investigation workflows.

## How It Works

When a cutoff timestamp is set via the `set_time_travel` tool:

- **File reads** are pinned to the last commit before the cutoff (SHA resolved automatically)
- **List endpoints** (issues, PRs, commits, releases, etc.) filter out items created after the cutoff
- **Issue and PR state** is rolled back (e.g., a merged PR appears open if it was merged after the cutoff)
- **Comments and reviews** created after the cutoff are hidden
- **Write operations** are blocked entirely

When the cutoff is cleared, all tools return to normal behavior.

## Setup

### Prerequisites

- Go 1.24+ installed
- A [GitHub Personal Access Token](https://github.com/settings/personal-access-tokens/new) with `repo` scope

### Build

```bash
git clone https://github.com/jcorte-real/github-mcp-server.git
cd github-mcp-server
go build -o github-mcp-server ./cmd/github-mcp-server/
```

### Configure in Claude Code

```bash
claude mcp add-json github-timemasking '{
  "command": "/absolute/path/to/github-mcp-server",
  "args": ["stdio"],
  "env": {
    "GITHUB_PERSONAL_ACCESS_TOKEN": "YOUR_PAT"
  }
}'
```

Replace `/absolute/path/to/github-mcp-server` with the actual path to the built binary.

To verify:

```bash
claude mcp list
claude mcp get github-timemasking
```

### Configure in Claude Desktop

Add to your `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "github-timemasking": {
      "command": "/absolute/path/to/github-mcp-server",
      "args": ["stdio"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "YOUR_PAT"
      }
    }
  }
}
```

Config file locations:
- **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows**: `%APPDATA%\Claude\claude_desktop_config.json`
- **Linux**: `~/.config/Claude/claude_desktop_config.json`

## Usage

Once configured, the agent has access to a `set_time_travel` tool.

### Enable time travel

Call `set_time_travel` with an ISO 8601 timestamp:

```
set_time_travel(cutoff: "2024-11-15T14:30:00Z")
```

All subsequent tool calls will return data as it existed at that timestamp.

### Disable time travel

```
set_time_travel(cutoff: "")
```

### Example: Investigate a past incident

```
1. set_time_travel(cutoff: "2024-11-15T14:30:00Z")
2. list_issues(owner: "myorg", repo: "myrepo", state: "open")
3. get_pull_request(owner: "myorg", repo: "myrepo", pullNumber: 42)
4. get_file_contents(owner: "myorg", repo: "myrepo", path: "src/config.yaml")
5. set_time_travel(cutoff: "")
```

At step 2, issues closed after the cutoff will appear as open. At step 3, a PR merged after the cutoff will appear as open with `merged: false`. At step 4, file contents are returned from the last commit before the cutoff.

## Affected Tools

### SHA override (file reads pinned to historical commit)
- `get_file_contents`

### List filtering (items after cutoff removed)
- `list_commits`
- `list_pull_requests`
- `list_issues`
- `list_releases`
- `list_discussions`
- `list_gists`
- `list_code_scanning_alerts`
- `list_secret_scanning_alerts`
- `list_dependabot_alerts`
- `list_repository_security_advisories`
- `list_org_repository_security_advisories`
- `list_starred_repositories`
- `projects_list`

### State masking (state rolled back if changed after cutoff)
- `issue_read` (method: `get`) -- closed_at nulled, state set to "open"
- `pull_request_read` (method: `get`) -- merged_at/closed_at nulled, merged set to false, state set to "open"

### Comment/review filtering
- `issue_read` (method: `get_comments`) -- filtered by created_at
- `pull_request_read` (method: `get_reviews`) -- filtered by submitted_at
- `pull_request_read` (method: `get_comments`) -- filtered by created_at

### Existence checks (single item hidden if created after cutoff)
- `get_commit`
- `get_gist`
- `get_code_scanning_alert`
- `get_secret_scanning_alert`
- `get_dependabot_alert`
- `get_release_by_tag`
- `get_latest_release`

### Write blocking
All tools without `ReadOnlyHint: true` are blocked when time travel is active.

## Running Tests

### Unit tests

```bash
go test ./pkg/github/ -run 'TimeMasking\|TimeTravel\|WriteBlocked\|FilterResponse\|StateMask'
```

### Integration tests

Integration tests run against the fixture repository [PagerDuty/github-mcp-server-time-travel-fixtures](https://github.com/PagerDuty/github-mcp-server-time-travel-fixtures). They require a `GITHUB_TOKEN` with access to that repo.

```bash
GITHUB_TOKEN=your_token go test -tags integration -run 'TestTimeTravel_Integration' -v ./pkg/github/
```

Or using a `.env` file:

```bash
echo 'GITHUB_TOKEN=your_token' > .env
export $(cat .env | xargs) && go test -tags integration -run 'TestTimeTravel_Integration' -v ./pkg/github/
```

## Design

See [docs/time-masking-proposal.md](docs/time-masking-proposal.md) for the full design document.
