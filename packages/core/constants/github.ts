export const DEFAULT_GITHUB_REPO = "multica-ai/multica";
export const DEFAULT_GITHUB_BRANCH = "main";

export function githubWebUrl(repo = DEFAULT_GITHUB_REPO): string {
  return `https://github.com/${repo}`;
}

export function githubIssuesUrl(repo = DEFAULT_GITHUB_REPO): string {
  return `${githubWebUrl(repo)}/issues`;
}

export function githubReleasesUrl(repo = DEFAULT_GITHUB_REPO): string {
  return `${githubWebUrl(repo)}/releases`;
}

export function githubReleasesApiUrl(repo = DEFAULT_GITHUB_REPO): string {
  return `https://api.github.com/repos/${repo}/releases`;
}

export function githubReleasesLatestApiUrl(repo = DEFAULT_GITHUB_REPO): string {
  return `${githubReleasesApiUrl(repo)}/latest`;
}

export function githubReleasesListApiUrl(
  repo = DEFAULT_GITHUB_REPO,
  perPage = 2,
): string {
  return `${githubReleasesApiUrl(repo)}?per_page=${perPage}`;
}

export function githubReleasesLatestDownloadUrl(
  repo = DEFAULT_GITHUB_REPO,
): string {
  return `${githubReleasesUrl(repo)}/latest/download`;
}

export function buildCliInstallCommand(
  repo = DEFAULT_GITHUB_REPO,
  branch = DEFAULT_GITHUB_BRANCH,
): string {
  const curl = `curl -fsSL https://raw.githubusercontent.com/${repo}/${branch}/scripts/install.sh | bash`;
  if (repo === DEFAULT_GITHUB_REPO) {
    return curl;
  }
  return `MULTICA_GITHUB_REPO=${repo} ${curl}`;
}

export function buildCliInstallPs1Command(
  repo = DEFAULT_GITHUB_REPO,
  branch = DEFAULT_GITHUB_BRANCH,
): string {
  return `irm https://raw.githubusercontent.com/${repo}/${branch}/scripts/install.ps1 | iex`;
}
