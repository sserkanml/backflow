#!/usr/bin/env python3
"""The provider layer of the hack/ e2e scripts: one command line for GitLab and GitHub.

    scm.py METHOD PATH [BODY]          call the Git host; prints the answer, exits 22 on HTTP >= 400
    scm.py STATUS-METHOD PATH [BODY]   print only the HTTP status
    scm.py seed-local DIR PREFIX WHAT  add the files of DIR below PREFIX when the demo repository lacks them
    scm.py seed-remote PROJECT         the same for the files of the public GitLab project PROJECT
    scm.py cleanup                     close the open merge requests that carry the label backflow-e2e (or come
                                       from a backflow/ branch) and delete the backflow/ branches; never main

The scripts speak one vocabulary, the one of the GitLab API: paths like
/projects/<url-encoded project>/merge_requests/3 and answers shaped like GitLab's
(iid, state opened/closed/merged, web_url, merge_commit_sha, commit.id, ...). With
SCM=github this program translates each call into the GitHub API and the answer
back, so every script, retry and check below it is the same for both hosts:

    GitLab                                     GitHub
    GET  /user                                 GET /user
    GET  /projects/P                           GET /repos/P (permissions -> access level)
    GET  .../repository/branches[/B]           git refs and branches
    DEL  .../repository/branches/B             DELETE git/refs/heads/B
    GET  .../repository/commits/REF            GET commits
    POST .../repository/commits                trees, a commit and a ref update (see commit())
    GET  .../repository/files/F/raw?ref=       contents, raw
    GET  .../repository/tree?recursive=true    git/trees?recursive
    GET  .../merge_requests[/N]                pulls (a closed pull request with merged_at is merged)
    PUT  .../merge_requests/N {state_event}    PATCH pulls/N
    PUT  .../merge_requests/N/merge            PUT pulls/N/merge (merge commit)
    GET  .../merge_requests/N/notes            issue comments
    GET, DEL .../labels/L                      labels/L
    GET  .../pipelines                         actions/runs (workflow runs; needs Actions: read)

The token is read from GITLAB_TOKEN or GITHUB_TOKEN, only from the environment, and
is never printed. Reads and idempotent writes are retried with a growing delay
(SCM_ATTEMPTS, SCM_TIMEOUT, SCM_BACKOFF; the GITLAB_* names still work). A write whose
outcome is unknown after a timeout or a 5xx (a commit, a merge) is repeated only
after checking that it did not happen: a commit is looked for at the head of its
branch (same message, on top of the head it was made against), a merge by the
state of the merge request.
"""
import base64
import http.client
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SCM = os.environ.get("SCM", "gitlab")
if SCM not in ("gitlab", "github"):
    sys.exit("SCM must be gitlab or github, not %r" % SCM)


def env(name, legacy, default):
    return os.environ.get(name) or os.environ.get(legacy) or default


ATTEMPTS = int(env("SCM_ATTEMPTS", "GITLAB_ATTEMPTS", "4"))
TIMEOUT = float(env("SCM_TIMEOUT", "GITLAB_TIMEOUT", "60"))
BACKOFF = float(env("SCM_BACKOFF", "GITLAB_BACKOFF", "3"))
RETRIABLE = (0, 429, 500, 502, 503, 504)


def log(msg):
    sys.stderr.write("%s: %s\n" % (SCM, msg))


def retriable(code, text):
    """gitlab.com sometimes answers 400, 502 or 503 with "Deadline Exceeded" when a
    commit takes too long on its side; GitHub answers 403 or 429 for a rate limit.
    The commit may or may not have landed: send() checks that before repeating one."""
    low = text.lower()
    if code in RETRIABLE:
        return True
    if code in (400, 502, 503) and "deadline exceeded" in low:
        return True
    return code == 403 and ("rate limit" in low or "abuse" in low)


def quote(s, safe=""):
    return urllib.parse.quote(s, safe=safe)


def pull(path, name):
    """The query parameter name of a path, or None."""
    return urllib.parse.parse_qs(urllib.parse.urlsplit(path).query).get(name, [None])[0]


# ---------------------------------------------------------------------------
# HTTP
# ---------------------------------------------------------------------------

def http_once(method, url, headers, body=None):
    """One attempt. Returns (status, text, retry_after); status 0 is a network
    error or a timeout (text says which)."""
    data = body.encode() if isinstance(body, str) else body
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        resp = urllib.request.urlopen(req, timeout=TIMEOUT)
        return resp.status, resp.read().decode(), None
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(), e.headers.get("Retry-After")
    except (urllib.error.URLError, TimeoutError, OSError, http.client.HTTPException) as e:
        return 0, "%s: %s" % (type(e).__name__, e), None


class GitLab:
    """Passes the call through."""

    def __init__(self):
        self.base = os.environ.get("GITLAB_API", "https://gitlab.com/api/v4")
        self.token = os.environ["GITLAB_TOKEN"]

    def once(self, method, path, body=None):
        headers = {"PRIVATE-TOKEN": self.token, "Content-Type": "application/json"}
        return http_once(method, self.base + path, headers, body)


class GitHub:
    """Translates a call in the vocabulary of the GitLab API into GitHub's."""

    def __init__(self):
        self.base = os.environ.get("GITHUB_API", "https://api.github.com")
        self.token = os.environ["GITHUB_TOKEN"]

    # -- plumbing ----------------------------------------------------------

    def call(self, method, path, body=None, accept="application/vnd.github+json"):
        """One request to GitHub: (status, text, retry_after)."""
        headers = {
            "Authorization": "Bearer " + self.token,
            "Accept": accept,
            "X-GitHub-Api-Version": "2022-11-28",
            "Content-Type": "application/json",
        }
        return http_once(method, self.base + path, headers, None if body is None else json.dumps(body))

    def json(self, method, path, body=None):
        """A request whose answer is JSON: (status, data, retry_after, text)."""
        code, text, retry = self.call(method, path, body)
        try:
            data = json.loads(text) if text else None
        except ValueError:
            data = None
        return code, data, retry, text

    def pages(self, path):
        """Every page of a list (up to 20 pages of 100): (status, items, retry_after, text)."""
        items = []
        sep = "&" if "?" in path else "?"
        for page in range(1, 21):
            code, data, retry, text = self.json("GET", "%s%sper_page=100&page=%d" % (path, sep, page))
            if code != 200 or not isinstance(data, list):
                return code, None, retry, text
            items += data
            if len(data) < 100:
                break
        return 200, items, None, ""

    @staticmethod
    def reply(code, data, retry=None):
        return code, json.dumps(data), retry

    # -- the translation ---------------------------------------------------

    def once(self, method, path, body=None):
        if path == "/user":
            return self.call(method, "/user")
        m = re.fullmatch(r"/projects/([^/?]+)([^?]*)(\?.*)?", path)
        if not m:
            return 501, "scm.py: %s %s is not supported for github" % (method, path), None
        repo = urllib.parse.unquote(m.group(1))
        rest = m.group(2)
        r = "/repos/" + repo
        spec = json.loads(body) if body else {}

        if rest == "" and method == "GET":
            return self.project(r)
        if rest == "/repository/branches" and method == "GET":
            return self.branches(r, pull(path, "search"))
        if (b := re.fullmatch(r"/repository/branches/(.+)", rest)):
            branch = urllib.parse.unquote(b.group(1))
            if method == "GET":
                return self.branch(r, branch)
            if method == "DELETE":
                return self.delete_branch(r, branch)
        if (c := re.fullmatch(r"/repository/commits/(.+)", rest)) and method == "GET":
            return self.commit_info(r, urllib.parse.unquote(c.group(1)))
        if rest == "/repository/commits" and method == "POST":
            return self.commit(r, spec)
        if (f := re.fullmatch(r"/repository/files/(.+)/raw", rest)) and method == "GET":
            return self.raw_file(r, urllib.parse.unquote(f.group(1)), pull(path, "ref"))
        if (f := re.fullmatch(r"/repository/files/(.+)", rest)) and method in ("GET", "HEAD"):
            return self.file_exists(r, urllib.parse.unquote(f.group(1)), pull(path, "ref"), method)
        if rest == "/repository/tree" and method == "GET":
            return self.tree(r, pull(path, "ref") or "main")
        if rest == "/merge_requests" and method == "GET":
            return self.pulls(r, pull(path, "source_branch"), pull(path, "state") or "opened")
        if (n := re.fullmatch(r"/merge_requests/(\d+)", rest)):
            if method == "GET":
                return self.pull_request(r, n.group(1))
            if method == "PUT":
                return self.update_pull(r, n.group(1), spec)
        if (n := re.fullmatch(r"/merge_requests/(\d+)/merge", rest)) and method == "PUT":
            return self.merge(r, n.group(1))
        if (n := re.fullmatch(r"/merge_requests/(\d+)/notes", rest)) and method == "GET":
            return self.notes(r, n.group(1))
        if (lab := re.fullmatch(r"/labels/(.+)", rest)) and method in ("GET", "DELETE"):
            code, text, retry = self.call(method, "%s/labels/%s" % (r, quote(urllib.parse.unquote(lab.group(1)))))
            return code, text, retry
        if rest == "/pipelines" and method == "GET":
            return self.runs(r, pull(path, "updated_after"), pull(path, "per_page"))
        return 501, "scm.py: %s %s is not supported for github" % (method, path), None

    def project(self, r):
        code, data, retry, text = self.json("GET", r)
        if code != 200:
            return code, text, retry
        level = 10
        for flag, value in (("pull", 10), ("triage", 20), ("push", 30), ("maintain", 40), ("admin", 50)):
            if (data.get("permissions") or {}).get(flag):
                level = value
        return self.reply(200, {
            "id": data.get("id"), "path_with_namespace": data.get("full_name"), "web_url": data.get("html_url"),
            "default_branch": data.get("default_branch"),
            "permissions": {"project_access": {"access_level": level}, "group_access": None},
        })

    def branch(self, r, branch):
        code, data, retry, text = self.json("GET", "%s/git/ref/heads/%s" % (r, quote(branch, "/")))
        if code != 200:
            return code, text, retry
        return self.reply(200, {"name": branch, "commit": {"id": data["object"]["sha"]}})

    def branches(self, r, search):
        code, items, retry, text = self.pages(r + "/branches")
        if code != 200:
            return code, text, retry
        return self.reply(200, [{"name": b["name"], "commit": {"id": b["commit"]["sha"]}}
                                for b in items if not search or search in b["name"]])

    def delete_branch(self, r, branch):
        code, text, retry = self.call("DELETE", "%s/git/refs/heads/%s" % (r, quote(branch, "/")))
        # GitHub answers 422 "Reference does not exist" for a branch that is gone.
        if code == 422 and "does not exist" in text.lower():
            return 404, text, retry
        return code, text, retry

    def sha_of(self, r, ref):
        """The commit a branch, tag or SHA names: (status, sha, retry_after, text)."""
        if re.fullmatch(r"[0-9a-f]{40}", ref):
            return 200, ref, None, ""
        code, data, retry, text = self.json("GET", "%s/git/ref/heads/%s" % (r, quote(ref, "/")))
        if code != 200:
            return code, None, retry, text
        return 200, data["object"]["sha"], None, ""

    def commit_info(self, r, ref):
        code, sha, retry, text = self.sha_of(r, ref)
        if code != 200:
            return code, text, retry
        code, data, retry, text = self.json("GET", "%s/commits/%s" % (r, sha))
        if code != 200:
            return code, text, retry
        message = data["commit"]["message"]
        return self.reply(200, {
            "id": data["sha"], "message": message, "title": message.split("\n")[0],
            "committer_name": (data["commit"].get("committer") or {}).get("name", ""),
            "author_name": (data["commit"].get("author") or {}).get("name", ""),
            "parent_ids": [p["sha"] for p in data.get("parents", [])],
        })

    def commit(self, r, spec):
        """POST .../repository/commits: GitHub has no call that changes several
        files at once, so the commit is built from a tree (the files of the head
        with these actions applied), a commit on the head, and a fast-forward of the
        branch. The steps are single attempts: when one fails or its answer is lost,
        send() looks at the head of the branch before it repeats the commit, and a
        tree or a commit that nobody points to is harmless. An empty repository has
        no tree to build on; there the files are created one by one."""
        branch, message, actions = spec["branch"], spec["commit_message"], spec["actions"]
        code, head, retry, text = self.sha_of(r, branch)
        if code == 404 and self.empty(r):
            return self.seed_empty(r, branch, message, actions)
        if code != 200:
            return code, text, retry
        code, parent, retry, text = self.json("GET", "%s/git/commits/%s" % (r, head))
        if code != 200:
            return code, text, retry
        entries = []
        for a in actions:
            if a["action"] in ("create", "update"):
                if a.get("encoding") == "base64":
                    return 400, "scm.py: base64 content is not supported for github", None
                entries.append({"path": a["file_path"], "mode": "100644", "type": "blob", "content": a["content"]})
            elif a["action"] == "delete":
                entries.append({"path": a["file_path"], "mode": "100644", "type": "blob", "sha": None})
            else:
                return 400, "scm.py: the action %r is not supported for github" % a["action"], None
        code, tree, retry, text = self.json("POST", r + "/git/trees", {"base_tree": parent["tree"]["sha"], "tree": entries})
        if code != 201:
            return code, text, retry
        code, made, retry, text = self.json("POST", r + "/git/commits",
                                            {"message": message, "tree": tree["sha"], "parents": [head]})
        if code != 201:
            return code, text, retry
        code, _, retry, text = self.json("PATCH", "%s/git/refs/heads/%s" % (r, quote(branch, "/")),
                                         {"sha": made["sha"], "force": False})
        if code != 200:
            # 422 "not a fast forward": the branch moved; the caller sees a conflict.
            return (409 if code == 422 else code), text, retry
        return self.reply(201, {"id": made["sha"], "message": message, "parent_ids": [head]})

    def empty(self, r):
        code, _, _, text = self.json("GET", r + "/commits?per_page=1")
        return code == 409 and "empty" in text.lower()

    def seed_empty(self, r, branch, message, actions):
        """The first file of an empty repository goes in through the contents API,
        which creates the branch; the rest follows as one ordinary commit."""
        creates = [a for a in actions if a["action"] == "create"]
        if len(creates) != len(actions) or not creates:
            return 400, "scm.py: an empty repository takes only created files", None
        first, rest = creates[0], creates[1:]
        body = {"message": message, "branch": branch, "content": base64.b64encode(first["content"].encode()).decode()}
        code, data, retry, text = self.json("PUT", "%s/contents/%s" % (r, quote(first["file_path"], "/")), body)
        if code not in (200, 201):
            return code, text, retry
        if not rest:
            return self.reply(201, {"id": data["commit"]["sha"], "message": message, "parent_ids": []})
        return self.commit(r, {"branch": branch, "commit_message": message, "actions": rest})

    def raw_file(self, r, path, ref):
        q = "?ref=" + quote(ref) if ref else ""
        return self.call("GET", "%s/contents/%s%s" % (r, quote(path, "/"), q), accept="application/vnd.github.raw+json")

    def file_exists(self, r, path, ref, method):
        q = "?ref=" + quote(ref) if ref else ""
        code, data, retry, text = self.json("GET", "%s/contents/%s%s" % (r, quote(path, "/"), q))
        if code != 200:
            return code, text, retry
        return 200, "" if method == "HEAD" else json.dumps({"file_path": path}), None

    def tree(self, r, ref):
        code, data, retry, text = self.json("GET", "%s/git/trees/%s?recursive=1" % (r, quote(ref, "/")))
        if code != 200:
            return code, text, retry
        return self.reply(200, [{"path": t["path"], "name": t["path"].rsplit("/", 1)[-1], "type": t["type"]}
                                for t in data.get("tree", [])])

    @staticmethod
    def merge_request(p):
        """A pull request in the shape of a GitLab merge request."""
        if p.get("merged_at") or p.get("merged"):
            state = "merged"
        elif p["state"] == "open":
            state = "opened"
        else:
            state = "closed"
        return {
            "iid": p["number"], "id": p["id"], "state": state, "web_url": p["html_url"], "title": p["title"],
            "description": p.get("body") or "", "source_branch": p["head"]["ref"], "target_branch": p["base"]["ref"],
            "sha": p["head"]["sha"], "merge_commit_sha": p.get("merge_commit_sha") if state == "merged" else None,
            "labels": [lab["name"] for lab in p.get("labels", [])], "force_remove_source_branch": False,
        }

    def pulls(self, r, source_branch, state):
        owner = r.split("/")[2]
        q = "?state=%s&sort=created&direction=desc" % ("open" if state == "opened" else "all" if state == "all" else "closed")
        if source_branch:
            q += "&head=" + quote("%s:%s" % (owner, source_branch))
        code, items, retry, text = self.pages(r + "/pulls" + q)
        if code != 200:
            return code, text, retry
        found = [self.merge_request(p) for p in items]
        if state == "merged":
            found = [p for p in found if p["state"] == "merged"]
        return self.reply(200, found)

    def pull_request(self, r, number):
        code, data, retry, text = self.json("GET", "%s/pulls/%s" % (r, number))
        if code != 200:
            return code, text, retry
        return self.reply(200, self.merge_request(data))

    def update_pull(self, r, number, spec):
        event = spec.get("state_event")
        if event not in ("close", "reopen"):
            return 400, "scm.py: only state_event close or reopen is supported for github", None
        code, data, retry, text = self.json("PATCH", "%s/pulls/%s" % (r, number),
                                            {"state": "closed" if event == "close" else "open"})
        if code != 200:
            return code, text, retry
        return self.reply(200, self.merge_request(data))

    def merge(self, r, number):
        """A merge commit, like GitLab's default. GitHub works out whether a pull
        request can be merged when it is asked for, so it is read first, and a refusal
        for a base that moved is retried a few times."""
        for _ in range(10):
            code, data, retry, text = self.json("GET", "%s/pulls/%s" % (r, number))
            if code != 200:
                return code, text, retry
            if data.get("mergeable") is not None:
                break
            time.sleep(1)
        for attempt in range(4):
            code, merged, retry, text = self.json("PUT", "%s/pulls/%s/merge" % (r, number), {"merge_method": "merge"})
            if code in (405, 409) and attempt < 3:
                time.sleep(3)
                continue
            break
        if code != 200:
            return code, text, retry
        return self.reply(200, {"iid": int(number), "state": "merged", "merge_commit_sha": merged["sha"]})

    def notes(self, r, number):
        code, items, retry, text = self.pages("%s/issues/%s/comments" % (r, number))
        if code != 200:
            return code, text, retry
        return self.reply(200, [{"id": c["id"], "body": c["body"], "system": False} for c in items])

    def runs(self, r, updated_after, per_page):
        q = "?per_page=%s" % (per_page or "100")
        if updated_after:
            q += "&created=" + quote(">=" + updated_after)
        code, data, retry, text = self.json("GET", "%s/actions/runs%s" % (r, q))
        if code != 200:
            return code, text, retry
        return self.reply(200, [{"id": w["id"], "ref": w.get("head_branch") or "", "status": w.get("status"),
                                 "sha": w.get("head_sha")} for w in data.get("workflow_runs", [])])


provider = None


def once(method, path, body=None):
    global provider
    if provider is None:
        provider = GitHub() if SCM == "github" else GitLab()
    return provider.once(method, path, body)


# ---------------------------------------------------------------------------
# Retries, and the checks that make a repeated write safe
# ---------------------------------------------------------------------------

def get_json(path):
    """A GET with retries; None when it cannot be read."""
    code, text = send("GET", path)
    if code >= 400 or code == 0:
        return None
    return json.loads(text)


def commit_check(pid, body):
    """For POST .../repository/commits: remember the head of the branch, and
    return a function that says whether this very commit has since landed on it."""
    spec = json.loads(body)
    branch, message = spec["branch"], spec["commit_message"].rstrip("\n")
    head_path = "/projects/%s/repository/branches/%s" % (pid, quote(branch))
    before = get_json(head_path)
    pre = before["commit"]["id"] if before else None

    def landed():
        now = get_json(head_path)
        if not now or not pre or now["commit"]["id"] == pre:
            return None
        info = get_json("/projects/%s/repository/commits/%s" % (pid, now["commit"]["id"]))
        if info and info.get("message", "").rstrip("\n") == message and pre in info.get("parent_ids", []):
            return 201, json.dumps(info)
        return None
    return landed


def merge_check(pid, iid):
    def merged():
        mr = get_json("/projects/%s/merge_requests/%s" % (pid, iid))
        if mr and mr.get("state") == "merged":
            return 200, json.dumps(mr)
        return None
    return merged


def send(method, path, body=None):
    """The request with retries. Returns (status, text)."""
    check = None
    if method == "POST" and (m := re.fullmatch(r"/projects/([^/]+)/repository/commits", path)):
        check = commit_check(m.group(1), body)
    elif method == "PUT" and (m := re.fullmatch(r"/projects/([^/]+)/merge_requests/(\d+)/merge", path)):
        check = merge_check(m.group(1), m.group(2))
    code, text, retry_after = 0, "", None
    for attempt in range(ATTEMPTS):
        if attempt:
            delay = BACKOFF * 2 ** (attempt - 1)
            if retry_after and retry_after.isdigit():
                delay = max(delay, min(int(retry_after), 60))
            time.sleep(delay)
            if check:  # the previous attempt may have worked although we did not see its answer
                done = check()
                if done:
                    log("%s %s: the earlier attempt had worked" % (method, path.split("?")[0]))
                    return done
        code, text, retry_after = once(method, path, body)
        if not retriable(code, text):
            # A DELETE that finds the thing gone after an unseen first attempt is done.
            if method == "DELETE" and attempt and code == 404:
                return 204, ""
            return code, text
        log("%s %s: attempt %d of %d failed (%s)" % (method, path.split("?")[0], attempt + 1, ATTEMPTS,
                                                     "HTTP %d" % code if code else text[:120]))
    if check:
        done = check()
        if done:
            log("%s %s: the last attempt had worked" % (method, path.split("?")[0]))
            return done
    return code, text


# ---------------------------------------------------------------------------
# Seeding the demo repository (dev-up.sh)
# ---------------------------------------------------------------------------

def seed(project, ref, files, what):
    """Commit the files (path -> content) that the repository lacks, with one
    commit. Never overwrites a file. Prints what it did."""
    pid = quote(project)
    actions, present = [], 0
    for rel, content in sorted(files.items()):
        code, text = send("GET", "/projects/%s/repository/files/%s?ref=%s" % (pid, quote(rel), quote(ref)))
        if code == 200:
            present += 1
        elif code in (404, 409):  # 409: GitHub says so for a repository without commits
            actions.append({"action": "create", "file_path": rel, "content": content})
        else:
            sys.exit("checking %s: HTTP %d" % (rel, code))
    if not actions:
        print("  %s is complete in the demo repository (%d files)" % (what, present))
        return
    body = json.dumps({"branch": ref, "commit_message": "demo: add " + what, "actions": actions})
    code, text = send("POST", "/projects/%s/repository/commits" % pid, body)
    if code >= 400 or code == 0:
        sys.exit("committing %d file(s): HTTP %d: %s" % (len(actions), code, text[:300]))
    print("  added %d file(s) to the demo repository: %s" % (len(actions), what))


def read_local(directory, prefix):
    files = {}
    for dirpath, _, names in os.walk(directory):
        for name in names:
            full = os.path.join(dirpath, name)
            rel = prefix + "/" + os.path.relpath(full, directory) if prefix else os.path.relpath(full, directory)
            with open(full, encoding="utf-8") as f:
                files[rel] = f.read()
    return files


def read_public_gitlab(project, ref="main"):
    """The files of a public GitLab project, read without a token."""
    api = os.environ.get("SEED_SOURCE_API", "https://gitlab.com/api/v4") + "/projects/" + quote(project)
    files, page = {}, 1
    while True:
        code, text, _ = http_once("GET", "%s/repository/tree?recursive=true&per_page=100&ref=%s&page=%d" % (api, quote(ref), page),
                                  {"Accept": "application/json"})
        if code != 200:
            sys.exit("reading the file list of %s: HTTP %d" % (project, code))
        tree = json.loads(text)
        for t in tree:
            if t["type"] == "blob":
                code, content, _ = http_once("GET", "%s/repository/files/%s/raw?ref=%s" % (api, quote(t["path"]), quote(ref)), {})
                if code != 200:
                    sys.exit("reading %s: HTTP %d" % (t["path"], code))
                files[t["path"]] = content
        if len(tree) < 100:
            return files
        page += 1


def cleanup(project, label="backflow-e2e", prefix="backflow/", rounds=10):
    """What an e2e run may leave behind: open merge requests of Backflow and its
    branches (the proposal branches backflow/<name> and the scratch branches
    backflow/scratch/...). Only names that start with prefix are touched, so main
    never is. Repeats until a listing shows nothing, because a list is one page."""
    pid = quote(project)
    closed = deleted = 0
    for _ in range(rounds):
        busy = False
        code, text = send("GET", "/projects/%s/merge_requests?state=opened&per_page=100" % pid)
        if code != 200:
            sys.exit("listing the open merge requests: HTTP %d" % code)
        for mr in json.loads(text):
            if label in mr.get("labels", []) or mr.get("source_branch", "").startswith(prefix):
                code, _ = send("PUT", "/projects/%s/merge_requests/%s" % (pid, mr["iid"]), '{"state_event":"close"}')
                if code >= 400:
                    sys.exit("closing !%s: HTTP %d" % (mr["iid"], code))
                closed += 1
                busy = True
        code, text = send("GET", "/projects/%s/repository/branches?search=%s&per_page=100" % (pid, quote(prefix)))
        if code != 200:
            sys.exit("listing the branches: HTTP %d" % code)
        for b in json.loads(text):
            if b["name"].startswith(prefix):
                code, _ = send("DELETE", "/projects/%s/repository/branches/%s" % (pid, quote(b["name"])))
                if code >= 400 and code != 404:
                    sys.exit("deleting the branch %s: HTTP %d" % (b["name"], code))
                deleted += 1
                busy = True
        if not busy:
            break
    print("cleanup of %s: closed %d merge request(s), deleted %d branch(es)" % (project, closed, deleted))


def main(argv):
    if len(argv) >= 2 and argv[1] == "cleanup":
        cleanup(os.environ["SCM_PROJECT"])
        return 0
    if len(argv) >= 2 and argv[1] == "seed-local":
        directory, prefix, what = argv[2:5]
        seed(os.environ["SCM_PROJECT"], os.environ.get("SCM_REF", "main"), read_local(directory, prefix), what)
        return 0
    if len(argv) >= 2 and argv[1] == "seed-remote":
        source = argv[2]
        seed(os.environ["SCM_PROJECT"], os.environ.get("SCM_REF", "main"), read_public_gitlab(source),
             "the files of the demo repository %s" % source)
        return 0
    method, path = argv[1], argv[2]
    body = argv[3] if len(argv) > 3 else None
    status_only = method.startswith("STATUS-")
    if status_only:
        method = method[len("STATUS-"):]
    code, text = send(method, path, body)
    if status_only:
        print(code)
        return 0
    if code >= 400 or code == 0:
        sys.stderr.write("HTTP %d: %s\n" % (code, text[:300]))
        return 22
    sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
