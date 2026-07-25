package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Show live, non-secret workspace coordination context",
	Args:  cobra.NoArgs,
	RunE:  runCoordinationContext,
}

var issueURLCmd = &cobra.Command{
	Use:   "url <issue>",
	Short: "Print the canonical workspace-qualified web URL for an issue",
	Args:  exactArgs(1),
	RunE:  runIssueURL,
}

var issueInspectCmd = &cobra.Command{
	Use:   "inspect <issue>",
	Short: "Inspect issue ownership, runs, PR handoff, and safe actions",
	Args:  exactArgs(1),
	RunE:  runIssueInspect,
}

var issueRouteCmd = &cobra.Command{
	Use:   "route",
	Short: "Propose a safe project, duplicate, owner, and assignee route",
	Args:  cobra.NoArgs,
	RunE:  runIssueRoute,
}

func init() {
	contextCmd.Flags().String("output", "json", "Output format: json")
	issueURLCmd.Flags().String("output", "text", "Output format: text or json")
	issueInspectCmd.Flags().String("output", "json", "Output format: json")
	issueRouteCmd.Flags().String("title", "", "Prospective issue title (required)")
	issueRouteCmd.Flags().String("description", "", "Prospective issue description")
	issueRouteCmd.Flags().String("repo", "", "GitHub clone URL associated with the work")
	issueRouteCmd.Flags().String("project", "", "Project id or short id to evaluate explicitly")
	issueRouteCmd.Flags().String("output", "json", "Output format: json")

	issueCmd.AddCommand(issueURLCmd, issueInspectCmd, issueRouteCmd)
}

func runCoordinationContext(cmd *cobra.Command, _ []string) error {
	if err := requireCoordinationJSONOutput(cmd); err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	if client.WorkspaceID == "" {
		if _, err := requireWorkspaceID(cmd); err != nil {
			return err
		}
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var workspace map[string]any
	if err := client.GetJSON(ctx, "/api/workspaces/"+url.PathEscape(client.WorkspaceID), &workspace); err != nil {
		return fmt.Errorf("load workspace context: %w", err)
	}
	var projectResult map[string]any
	if err := client.GetJSON(ctx, "/api/projects?workspace_id="+url.QueryEscape(client.WorkspaceID), &projectResult); err != nil {
		return fmt.Errorf("load project context: %w", err)
	}
	rawProjects := mapSlice(projectResult["projects"])
	projects := make([]map[string]any, 0, len(rawProjects))
	repositories := make([]map[string]any, 0)
	for _, raw := range anySlice(workspace["repos"]) {
		if repo, ok := raw.(map[string]any); ok {
			repositories = append(repositories, map[string]any{
				"scope": "workspace", "url": strVal(repo, "url"), "description": strVal(repo, "description"),
			})
		}
	}
	for _, rawProject := range rawProjects {
		project := safeCoordinationProject(rawProject)
		projectID := strVal(project, "id")
		var resourceResult map[string]any
		if projectID != "" {
			if err := client.GetJSON(ctx, "/api/projects/"+url.PathEscape(projectID)+"/resources", &resourceResult); err != nil {
				return fmt.Errorf("load resources for project %s: %w", projectID, err)
			}
		}
		rawResources := mapSlice(resourceResult["resources"])
		resources := make([]map[string]any, 0, len(rawResources))
		for _, resource := range rawResources {
			resources = append(resources, safeCoordinationResource(resource))
		}
		project["resources"] = resources
		projects = append(projects, project)
		for _, resource := range resources {
			if strVal(resource, "resource_type") != "github_repo" {
				continue
			}
			ref, _ := resource["resource_ref"].(map[string]any)
			repositories = append(repositories, map[string]any{
				"scope": "project", "project_id": projectID, "resource_id": strVal(resource, "id"), "url": strVal(ref, "url"),
			})
		}
	}

	var rawAgents []map[string]any
	agentPath := "/api/agents?workspace_id=" + url.QueryEscape(client.WorkspaceID)
	if err := client.GetJSON(ctx, agentPath, &rawAgents); err != nil {
		return fmt.Errorf("load routing agents: %w", err)
	}
	var taskSnapshot []map[string]any
	if err := client.GetJSON(ctx, "/api/agent-task-snapshot", &taskSnapshot); err != nil {
		return fmt.Errorf("load agent availability: %w", err)
	}
	activeCounts := map[string]int{}
	for _, task := range taskSnapshot {
		switch strVal(task, "status") {
		case "queued", "dispatched", "running", "waiting_local_directory":
			activeCounts[strVal(task, "agent_id")]++
		}
	}
	agents := make([]map[string]any, 0, len(rawAgents))
	for _, agent := range rawAgents {
		id := strVal(agent, "id")
		capacity := numberVal(agent["max_concurrent_tasks"])
		if capacity <= 0 {
			capacity = 1
		}
		status := strVal(agent, "status")
		available := status != "offline" && status != "archived" && activeCounts[id] < capacity
		agents = append(agents, map[string]any{
			"id": id, "name": strVal(agent, "name"), "status": status,
			"runtime_id": strVal(agent, "runtime_id"), "max_concurrent_tasks": capacity,
			"active_task_count": activeCounts[id], "available": available,
		})
	}

	profile := resolveProfile(cmd)
	if profile == "" {
		profile = "default"
	}
	return cli.PrintJSON(os.Stdout, map[string]any{
		"profile": profile,
		"app_url": tryResolveAppURL(cmd),
		"workspace": map[string]any{
			"id": strVal(workspace, "id"), "slug": strVal(workspace, "slug"), "name": strVal(workspace, "name"),
		},
		"projects":     projects,
		"repositories": repositories,
		"agents":       agents,
	})
}

func runIssueURL(cmd *cobra.Command, args []string) error {
	appURL := tryResolveAppURL(cmd)
	if appURL == "" {
		return fmt.Errorf("app URL not set: set MULTICA_APP_URL or run 'multica config set app_url <url>'")
	}
	parsed, err := url.Parse(appURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("configured app URL must be an absolute http(s) URL")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var workspace map[string]any
	if err := client.GetJSON(ctx, "/api/workspaces/"+url.PathEscape(client.WorkspaceID), &workspace); err != nil {
		return fmt.Errorf("load issue workspace: %w", err)
	}
	workspaceSlug := strings.TrimSpace(strVal(workspace, "slug"))
	if workspaceSlug == "" {
		return fmt.Errorf("issue workspace has no URL slug")
	}
	parsed.Path = path.Join(parsed.Path, workspaceSlug, "issues", issueRef.ID)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	canonical := parsed.String()
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		return cli.PrintJSON(os.Stdout, map[string]any{"issue_id": issueRef.ID, "identifier": issueRef.Display, "url": canonical})
	}
	if output != "text" {
		return fmt.Errorf("invalid --output %q; valid values: text, json", output)
	}
	fmt.Fprintln(os.Stdout, canonical)
	return nil
}

func runIssueInspect(cmd *cobra.Command, args []string) error {
	if err := requireCoordinationJSONOutput(cmd); err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	issueRef, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var result map[string]any
	if err := client.GetJSON(ctx, "/api/issues/"+url.PathEscape(issueRef.ID)+"/inspect", &result); err != nil {
		return fmt.Errorf("inspect issue: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func runIssueRoute(cmd *cobra.Command, _ []string) error {
	if err := requireCoordinationJSONOutput(cmd); err != nil {
		return err
	}
	title, _ := cmd.Flags().GetString("title")
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("--title is required")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	body := map[string]any{"title": title}
	if description, _ := cmd.Flags().GetString("description"); description != "" {
		body["description"] = description
	}
	if repo, _ := cmd.Flags().GetString("repo"); repo != "" {
		body["repository"] = strings.TrimSpace(repo)
	}
	if project, _ := cmd.Flags().GetString("project"); project != "" {
		resolved, err := resolveProjectID(ctx, client, project)
		if err != nil {
			return fmt.Errorf("resolve project: %w", err)
		}
		body["project_id"] = resolved.ID
	}
	var result map[string]any
	if err := client.PostJSON(ctx, "/api/issues/route", body, &result); err != nil {
		return fmt.Errorf("route issue: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

func mapSlice(value any) []map[string]any {
	items := anySlice(value)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func safeCoordinationProject(project map[string]any) map[string]any {
	return map[string]any{
		"id":             strVal(project, "id"),
		"short_id":       strVal(project, "short_id"),
		"title":          strVal(project, "title"),
		"description":    strVal(project, "description"),
		"status":         strVal(project, "status"),
		"priority":       strVal(project, "priority"),
		"resource_count": numberVal(project["resource_count"]),
	}
}

func safeCoordinationResource(resource map[string]any) map[string]any {
	resourceType := strVal(resource, "resource_type")
	out := map[string]any{
		"id":            strVal(resource, "id"),
		"resource_type": resourceType,
		"label":         strVal(resource, "label"),
		"position":      numberVal(resource["position"]),
	}
	ref, _ := resource["resource_ref"].(map[string]any)
	safeRef := map[string]any{}
	switch resourceType {
	case "github_repo":
		safeRef["url"] = strVal(ref, "url")
		safeRef["ref"] = strVal(ref, "ref")
		safeRef["default_branch_hint"] = strVal(ref, "default_branch_hint")
	case "local_directory":
		safeRef["local_path"] = strVal(ref, "local_path")
		safeRef["daemon_id"] = strVal(ref, "daemon_id")
		safeRef["label"] = strVal(ref, "label")
	}
	out["resource_ref"] = safeRef
	return out
}

func requireCoordinationJSONOutput(cmd *cobra.Command) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "json" {
		return fmt.Errorf("invalid --output %q; valid value: json", output)
	}
	return nil
}

func numberVal(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int32:
		return int(v)
	}
	return 0
}
