"""Generate release notes from commit subjects since the previous published release."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import quote


def git(*args):
    return subprocess.check_output(["git", *args], text=True, encoding="utf-8").strip()


def api_pages(endpoint):
    pages = json.loads(subprocess.check_output(
        ["gh", "api", "--paginate", "--slurp", endpoint],
        text=True, encoding="utf-8",
    ))
    return [item for page in pages for item in page]


def generate(repository, tag, sha, releases, pull_requests=None):
    previous = None
    for release in sorted(releases, key=lambda r: r.get("published_at") or "", reverse=True):
        candidate = release["tag_name"]
        if release["draft"] or candidate == tag or ("-" not in tag and release["prerelease"]):
            continue
        ref = f"refs/tags/{candidate}^{{commit}}"
        resolved = subprocess.run(["git", "rev-parse", "--verify", ref], capture_output=True, text=True)
        if resolved.returncode or resolved.stdout.strip() == sha:
            continue
        if subprocess.run(["git", "merge-base", "--is-ancestor", ref, sha]).returncode == 0:
            previous = candidate
            break
    revision = f"refs/tags/{previous}..{sha}" if previous else sha
    entries = git("log", "--no-merges", "--reverse", "--format=%H%x09%s", revision)
    lines = ["## 更新内容", ""]
    credited = set()
    eligible = {}
    for entry in entries.splitlines():
        commit, subject = entry.split("\t", 1)
        subject = re.sub(r"([\\`*_{}\[\]()<>#!|~])", r"\\\1", subject)
        subject = re.sub(r"(?<![A-Za-z0-9_.+@-])@[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)?", r"`\g<0>`", subject)
        attribution = []
        for pr in sorted(pull_requests(commit) if pull_requests else [], key=lambda p: p["number"]):
            number = pr["number"]
            if number in credited:
                continue
            if number not in eligible:
                user = pr.get("user") or {}
                login = user.get("login", "")
                merge_sha = pr.get("merge_commit_sha") or ""
                base_repo = ((pr.get("base") or {}).get("repo") or {}).get("full_name", "")
                eligible[number] = (
                    bool(pr.get("merged_at"))
                    and base_repo.lower() == repository.lower()
                    and user.get("type") != "Bot"
                    and re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})", login) is not None
                    and re.fullmatch(r"[0-9a-fA-F]{40}", merge_sha) is not None
                    and subprocess.run(["git", "merge-base", "--is-ancestor", merge_sha, sha], capture_output=True).returncode == 0
                    and (not previous or subprocess.run(
                        ["git", "merge-base", "--is-ancestor", merge_sha, f"refs/tags/{previous}"], capture_output=True,
                    ).returncode != 0)
                )
            if eligible[number]:
                attribution.append(f"[#{number}](https://github.com/{repository}/pull/{number}) by @{pr['user']['login']}")
                credited.add(number)
        suffix = f" · {'; '.join(attribution)}" if attribution else ""
        lines.append(f"- {subject} ([{commit[:7]}](https://github.com/{repository}/commit/{commit})){suffix}")
    if not entries:
        lines.append("- 无新增提交。")
    if previous:
        comparison = f"https://github.com/{repository}/compare/{quote(previous, safe='')}...{quote(tag, safe='')}"
        lines.extend(["", f"**完整变更**：[{previous}…{tag}]({comparison})"])
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    repository = os.environ["GITHUB_REPOSITORY"]
    releases = api_pages(f"repos/{repository}/releases?per_page=100")
    Path(sys.argv[1]).write_text(generate(
        repository, os.environ["RELEASE_TAG"], os.environ["RELEASE_SHA"], releases,
        lambda commit: api_pages(f"repos/{repository}/commits/{commit}/pulls?per_page=100"),
    ), encoding="utf-8")
