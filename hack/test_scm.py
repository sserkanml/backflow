#!/usr/bin/env python3
"""Tests of hack/scm.py against fake GitLab and GitHub servers.

    python3 hack/test_scm.py

No network and no token: the servers run on localhost and the token is a dummy.
What the tests pin down is the translation of the GitLab-shaped calls the e2e
scripts make into GitHub's API and back, and the retries.
"""
import hashlib
import json
import os
import subprocess
import sys
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
SCM_PY = os.path.join(HERE, "scm.py")
TOKEN = "dummy-token-for-tests"


def sha(*parts):
    return hashlib.sha1("|".join(parts).encode()).hexdigest()


class FakeGitHub:
    """Just enough of the GitHub API, in memory, for one repository o/r."""

    def __init__(self):
        self.commits = {}  # sha -> {"tree": {path: content}, "message", "parents"}
        self.heads = {}
        self.trees = {}
        self.pulls = {}
        self.comments = {}
        self.labels = {"backflow-e2e"}
        self.runs = []
        self.requests = []
        self.fail_next = []  # statuses to answer before behaving
        self.lock = threading.Lock()

    def commit(self, branch, message, files, parent=None):
        s = sha(message, branch, str(len(self.commits)))
        self.commits[s] = {"tree": dict(files), "message": message, "parents": [parent] if parent else []}
        self.heads[branch] = s
        return s


def make_handler(state):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def send(self, code, data=None, raw=None):
            body = raw if raw is not None else (json.dumps(data) if data is not None else "")
            self.send_response(code)
            self.send_header("Content-Length", str(len(body.encode())))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(body.encode())

        def handle_any(self):
            url = urllib.parse.urlsplit(self.path)
            path, q = urllib.parse.unquote(url.path), urllib.parse.parse_qs(url.query)
            length = int(self.headers.get("Content-Length") or 0)
            body = json.loads(self.rfile.read(length)) if length else {}
            with state.lock:
                state.requests.append((self.command, path, self.headers.get("Authorization"), self.headers.get("Accept")))
                if state.fail_next:
                    return self.send(state.fail_next.pop(0), {"message": "boom"})
                self.route(self.command, path, q, body)

        do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = handle_any

        def route(self, method, path, q, body):
            s = state
            if self.headers.get("Authorization") != "Bearer " + TOKEN:
                return self.send(401, {"message": "Bad credentials"})
            if path == "/user":
                return self.send(200, {"login": "tester"})
            m = path.split("/")
            if m[1] != "repos" or m[2] != "o" or m[3] != "r":
                return self.send(404, {"message": "Not Found"})
            rest = "/" + "/".join(m[4:])
            if rest == "/" and method == "GET":
                return self.send(200, {"id": 7, "full_name": "o/r", "html_url": "https://github.com/o/r",
                                       "default_branch": "main", "permissions": {"pull": True, "push": True}})
            if rest.startswith("/git/ref/heads/"):
                b = rest[len("/git/ref/heads/"):]
                return self.send(200, {"object": {"sha": s.heads[b]}}) if b in s.heads else self.send(404, {"message": "Not Found"})
            if rest == "/branches":
                names = sorted(s.heads)
                page, per = int(q.get("page", ["1"])[0]), int(q.get("per_page", ["30"])[0])
                return self.send(200, [{"name": n, "commit": {"sha": s.heads[n]}} for n in names][(page - 1) * per:page * per])
            if rest.startswith("/git/refs/heads/"):
                b = rest[len("/git/refs/heads/"):]
                if method == "DELETE":
                    if b not in s.heads:
                        return self.send(422, {"message": "Reference does not exist"})
                    del s.heads[b]
                    return self.send(204)
                if method == "PATCH":
                    cur = s.heads.get(b)
                    if cur is None or cur not in s.commits[body["sha"]]["parents"]:
                        return self.send(422, {"message": "Update is not a fast forward"})
                    s.heads[b] = body["sha"]
                    return self.send(200, {"object": {"sha": body["sha"]}})
            if rest == "/commits" and method == "GET":
                return self.send(200, [{"sha": x} for x in s.commits][:1]) if s.commits else self.send(409, {"message": "Git Repository is empty."})
            if rest.startswith("/commits/"):
                c = s.commits.get(rest[len("/commits/"):])
                if not c:
                    return self.send(404, {"message": "Not Found"})
                return self.send(200, {"sha": rest[len("/commits/"):], "commit": {"message": c["message"], "committer": {"name": "Backflow"}},
                                       "parents": [{"sha": p} for p in c["parents"]]})
            if rest.startswith("/git/commits/") and method == "GET":
                x = rest[len("/git/commits/"):]
                return self.send(200, {"sha": x, "tree": {"sha": sha("tree", x)}})
            if rest == "/git/trees" and method == "POST":
                parent = next(x for x in s.commits if sha("tree", x) == body["base_tree"])
                tree = dict(s.commits[parent]["tree"])
                for e in body["tree"]:
                    if e.get("sha", 1) is None:
                        tree.pop(e["path"], None)
                    else:
                        tree[e["path"]] = e["content"]
                t = sha("t", json.dumps(tree, sort_keys=True))
                s.trees[t] = tree
                return self.send(201, {"sha": t})
            if rest == "/git/commits" and method == "POST":
                c = sha(body["message"], body["tree"], str(len(s.commits)))
                s.commits[c] = {"tree": s.trees[body["tree"]], "message": body["message"], "parents": body["parents"]}
                # The tree of a commit is looked up by sha("tree", commit) above; make that work.
                return self.send(201, {"sha": c})
            if rest.startswith("/contents/"):
                p = rest[len("/contents/"):]
                if method == "PUT":
                    import base64
                    head = s.heads.get(body["branch"])
                    files = dict(s.commits[head]["tree"]) if head else {}
                    files[p] = base64.b64decode(body["content"]).decode()
                    c = s.commit(body["branch"], body["message"], files, head)
                    return self.send(201, {"commit": {"sha": c}})
                head = s.heads.get(q.get("ref", ["main"])[0])
                if head is None and q.get("ref", [""])[0] in s.commits:
                    head = q["ref"][0]
                if head is None or p not in s.commits[head]["tree"]:
                    return self.send(404, {"message": "Not Found"})
                if "raw" in (self.headers.get("Accept") or ""):
                    return self.send(200, raw=s.commits[head]["tree"][p])
                return self.send(200, {"path": p})
            if rest.startswith("/git/trees/"):
                head = s.heads.get(rest[len("/git/trees/"):])
                if head is None:
                    return self.send(404, {"message": "Not Found"})
                return self.send(200, {"tree": [{"path": p, "type": "blob"} for p in sorted(s.commits[head]["tree"])]})
            if rest == "/pulls" and method == "GET":
                items = list(s.pulls.values())
                if q.get("state", ["open"])[0] == "open":
                    items = [p for p in items if p["state"] == "open"]
                if "head" in q:
                    items = [p for p in items if "o:" + p["head"]["ref"] == q["head"][0]]
                items.sort(key=lambda p: -p["number"])
                return self.send(200, items)
            pr = rest.split("/")
            if len(pr) >= 3 and pr[1] == "pulls":
                p = s.pulls.get(int(pr[2]))
                if p is None:
                    return self.send(404, {"message": "Not Found"})
                if len(pr) == 3 and method == "GET":
                    return self.send(200, p)
                if len(pr) == 3 and method == "PATCH":
                    p["state"] = body["state"]
                    return self.send(200, p)
                if len(pr) == 4 and pr[3] == "merge":
                    if p["state"] != "open":
                        return self.send(405, {"message": "Pull Request is not mergeable"})
                    p["state"], p["merged_at"], p["merged"] = "closed", "2026-01-01T00:00:00Z", True
                    p["merge_commit_sha"] = sha("merge", str(p["number"]))
                    return self.send(200, {"sha": p["merge_commit_sha"], "merged": True})
            if len(pr) == 4 and pr[1] == "issues" and pr[3] == "comments":
                return self.send(200, s.comments.get(int(pr[2]), []))
            if len(pr) == 3 and pr[1] == "labels":
                if pr[2] not in s.labels:
                    return self.send(404, {"message": "Not Found"})
                if method == "GET":
                    return self.send(200, {"name": pr[2]})
                s.labels.discard(pr[2])
                return self.send(204)
            if rest == "/actions/runs":
                return self.send(200, {"total_count": len(s.runs), "workflow_runs": s.runs})
            return self.send(404, {"message": "Not Found " + rest})

    return Handler


class Server:
    def __init__(self, handler):
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        self.url = "http://127.0.0.1:%d" % self.httpd.server_address[1]
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def close(self):
        self.httpd.shutdown()
        self.httpd.server_close()


def run(scm, api, *args):
    env = dict(os.environ, SCM=scm, SCM_BACKOFF="0", SCM_ATTEMPTS="3", SCM_TIMEOUT="5")
    env["GITHUB_TOKEN" if scm == "github" else "GITLAB_TOKEN"] = TOKEN
    env["GITHUB_API" if scm == "github" else "GITLAB_API"] = api
    r = subprocess.run([sys.executable, SCM_PY, *args], capture_output=True, text=True, env=env)
    return r.returncode, r.stdout, r.stderr


def pr(number, branch, state="open", merged=False, labels=(), body="the description"):
    return {"id": 1000 + number, "number": number, "state": state, "html_url": "https://github.com/o/r/pull/%d" % number,
            "title": "backflow: sync", "body": body, "head": {"ref": branch, "sha": "h" * 40}, "base": {"ref": "main"},
            "merged_at": "2026-01-01T00:00:00Z" if merged else None, "merged": merged,
            "merge_commit_sha": "m" * 40 if merged else None, "labels": [{"name": n} for n in labels], "mergeable": True}


PID = urllib.parse.quote("o/r", safe="")


class GitHubTests(unittest.TestCase):
    def setUp(self):
        self.state = FakeGitHub()
        self.state.commit("main", "init", {"README.md": "hello\n", "apps/demo/configmap.yaml": "a: 1\n"})
        self.server = Server(make_handler(self.state))
        self.addCleanup(self.server.close)

    def call(self, *args):
        return run("github", self.server.url, *args)

    def json(self, *args):
        code, out, err = self.call(*args)
        self.assertEqual(code, 0, err)
        return json.loads(out)

    def test_the_token_is_sent_as_a_bearer_and_only_there(self):
        self.assertEqual(self.json("GET", "/user")["login"], "tester")
        self.assertEqual(self.state.requests[0][2], "Bearer " + TOKEN)
        code, _, err = run("github", self.server.url, "STATUS-GET", "/user")
        self.assertNotIn(TOKEN, err)

    def test_project_permissions_become_an_access_level(self):
        d = self.json("GET", "/projects/" + PID)
        self.assertEqual(d["permissions"]["project_access"]["access_level"], 30)
        self.assertIsNone(d["permissions"]["group_access"])

    def test_branches(self):
        self.state.commit("backflow/a-1", "x", {"README.md": "x"})
        self.assertEqual(self.json("GET", "/projects/%s/repository/branches/main" % PID)["commit"]["id"], self.state.heads["main"])
        names = [b["name"] for b in self.json("GET", "/projects/%s/repository/branches?search=backflow/&per_page=100" % PID)]
        self.assertEqual(names, ["backflow/a-1"])
        self.assertEqual(self.call("STATUS-GET", "/projects/%s/repository/branches/%s" % (PID, urllib.parse.quote("backflow/a-1", safe="")))[1].strip(), "200")
        self.assertEqual(self.call("STATUS-GET", "/projects/%s/repository/branches/nope" % PID)[1].strip(), "404")

    def test_deleting_a_branch_that_is_gone_is_a_404(self):
        enc = urllib.parse.quote("backflow/gone", safe="")
        self.assertEqual(self.call("STATUS-DELETE", "/projects/%s/repository/branches/%s" % (PID, enc))[1].strip(), "404")
        self.state.commit("backflow/x", "x", {"README.md": "x"})
        self.assertEqual(self.call("STATUS-DELETE", "/projects/%s/repository/branches/%s" % (PID, urllib.parse.quote("backflow/x", safe="")))[1].strip(), "204")
        self.assertNotIn("backflow/x", self.state.heads)

    def test_raw_file_and_tree(self):
        code, out, _ = self.call("GET", "/projects/%s/repository/files/%s/raw?ref=main" % (PID, urllib.parse.quote("apps/demo/configmap.yaml", safe="")))
        self.assertEqual((code, out), (0, "a: 1\n"))
        tree = self.json("GET", "/projects/%s/repository/tree?recursive=true&per_page=100&ref=main" % PID)
        self.assertEqual(sorted(t["path"] for t in tree if t["type"] == "blob"), ["README.md", "apps/demo/configmap.yaml"])
        self.assertEqual(self.call("STATUS-GET", "/projects/%s/repository/files/%s/raw?ref=main" % (PID, "nope"))[1].strip(), "404")

    def test_a_commit_with_several_actions(self):
        before = self.state.heads["main"]
        body = json.dumps({"branch": "main", "commit_message": "edit things\n", "actions": [
            {"action": "update", "file_path": "apps/demo/configmap.yaml", "content": "a: 2\n"},
            {"action": "create", "file_path": "new/file.yaml", "content": "n: 1\n"},
            {"action": "delete", "file_path": "README.md"},
        ]})
        made = self.json("POST", "/projects/%s/repository/commits" % PID, body)
        head = self.state.heads["main"]
        self.assertEqual(made["id"], head)
        self.assertNotEqual(head, before)
        self.assertEqual(self.state.commits[head]["tree"], {"apps/demo/configmap.yaml": "a: 2\n", "new/file.yaml": "n: 1\n"})
        info = self.json("GET", "/projects/%s/repository/commits/main" % PID)
        self.assertEqual((info["id"], info["message"], info["committer_name"], info["parent_ids"]), (head, "edit things\n", "Backflow", [before]))

    def test_a_commit_whose_answer_was_lost_is_not_repeated(self):
        # The branch moves, but the answer to the update is a 502.
        s = self.state
        body = json.dumps({"branch": "main", "commit_message": "once", "actions": [
            {"action": "update", "file_path": "README.md", "content": "changed\n"}]})
        handler = self.server.httpd.RequestHandlerClass
        orig_send = handler.send
        lost = []

        def send(h, code, data=None, raw=None):
            if h.command == "PATCH" and code == 200 and not lost:
                lost.append(1)
                return orig_send(h, 502, {"message": "Bad Gateway"})
            return orig_send(h, code, data, raw)
        handler.send = send
        self.addCleanup(setattr, handler, "send", orig_send)
        made = self.json("POST", "/projects/%s/repository/commits" % PID, body)
        self.assertEqual(len(s.commits), 2, "the commit exists once")
        self.assertEqual(made["id"], s.heads["main"])

    def test_the_first_files_of_an_empty_repository(self):
        s = FakeGitHub()
        server = Server(make_handler(s))
        self.addCleanup(server.close)
        body = json.dumps({"branch": "main", "commit_message": "demo: add x", "actions": [
            {"action": "create", "file_path": "README.md", "content": "hi\n"},
            {"action": "create", "file_path": "a/b.yaml", "content": "k: v\n"}]})
        code, out, err = run("github", server.url, "POST", "/projects/%s/repository/commits" % PID, body)
        self.assertEqual(code, 0, err)
        self.assertEqual(s.commits[s.heads["main"]]["tree"], {"README.md": "hi\n", "a/b.yaml": "k: v\n"})
        self.assertEqual(len(s.commits), 2, "one commit for the first file, one for the rest")

    def test_merge_requests(self):
        s = self.state
        s.pulls[1] = pr(1, "backflow/a", labels=["backflow-e2e"])
        s.pulls[2] = pr(2, "backflow/b", state="closed")
        s.pulls[3] = pr(3, "backflow/c", state="closed", merged=True)
        opened = self.json("GET", "/projects/%s/merge_requests?state=opened" % PID)
        self.assertEqual([m["iid"] for m in opened], [1])
        info = self.json("GET", "/projects/%s/merge_requests?source_branch=%s&state=all&order_by=created_at&sort=desc" % (PID, urllib.parse.quote("backflow/c", safe="")))
        self.assertEqual((info[0]["iid"], info[0]["state"], info[0]["merge_commit_sha"]), (3, "merged", "m" * 40))
        one = self.json("GET", "/projects/%s/merge_requests/1" % PID)
        self.assertEqual((one["state"], one["description"], one["labels"], one["target_branch"], one["force_remove_source_branch"]),
                         ("opened", "the description", ["backflow-e2e"], "main", False))
        self.assertEqual(self.json("GET", "/projects/%s/merge_requests/2" % PID)["state"], "closed")

    def test_closing_and_merging(self):
        s = self.state
        s.pulls[1] = pr(1, "backflow/a")
        s.pulls[2] = pr(2, "backflow/b")
        closed = self.json("PUT", "/projects/%s/merge_requests/1" % PID, '{"state_event":"close"}')
        self.assertEqual(closed["state"], "closed")
        merged = self.json("PUT", "/projects/%s/merge_requests/2/merge" % PID, '{"should_remove_source_branch":false}')
        self.assertEqual((merged["state"], merged["merge_commit_sha"]), ("merged", sha("merge", "2")))
        self.assertEqual(self.json("GET", "/projects/%s/merge_requests/2" % PID)["state"], "merged")
        self.assertEqual(self.call("PUT", "/projects/%s/merge_requests/1/merge" % PID, "{}")[0], 22)

    def test_notes_labels_and_pipelines(self):
        s = self.state
        s.comments[1] = [{"id": 1, "body": "hello"}]
        notes = self.json("GET", "/projects/%s/merge_requests/1/notes?per_page=50" % PID)
        self.assertEqual([(n["body"], n["system"]) for n in notes], [("hello", False)])
        self.assertEqual(self.call("STATUS-GET", "/projects/%s/labels/backflow-e2e" % PID)[1].strip(), "200")
        self.assertEqual(self.call("STATUS-DELETE", "/projects/%s/labels/backflow-e2e" % PID)[1].strip(), "204")
        self.assertEqual(self.call("STATUS-DELETE", "/projects/%s/labels/backflow-e2e" % PID)[1].strip(), "404")
        s.runs = [{"id": 5, "head_branch": "backflow/scratch/x", "status": "completed", "head_sha": "s" * 40}]
        runs = self.json("GET", "/projects/%s/pipelines?updated_after=2026-01-01T00:00:00Z&per_page=100" % PID)
        self.assertEqual([r["ref"] for r in runs], ["backflow/scratch/x"])

    def test_retries(self):
        self.state.fail_next = [503, 502]
        self.assertEqual(self.json("GET", "/user")["login"], "tester")
        self.state.fail_next = [503, 503, 503]
        code, _, err = self.call("GET", "/user")
        self.assertEqual(code, 22)
        self.assertIn("HTTP 503", err)
        self.state.fail_next = [403]
        self.assertEqual(self.call("GET", "/user")[0], 22)  # a refusal is not retried

    def test_cleanup_closes_and_deletes_only_what_is_backflows(self):
        s = self.state
        s.commit("backflow/a-1", "x", {"README.md": "x"})
        s.commit("backflow/scratch/ab12-1", "x", {"README.md": "x"})
        s.commit("feature/other", "x", {"README.md": "x"})
        s.pulls[1] = pr(1, "backflow/a-1", labels=["backflow-e2e"])
        s.pulls[2] = pr(2, "feature/other", labels=["something"])
        s.pulls[3] = pr(3, "feature/labelled", labels=["backflow-e2e"])
        env = dict(os.environ, SCM="github", GITHUB_TOKEN=TOKEN, GITHUB_API=self.server.url, SCM_PROJECT="o/r", SCM_BACKOFF="0")
        r = subprocess.run([sys.executable, SCM_PY, "cleanup"], capture_output=True, text=True, env=env)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("closed 2 merge request(s), deleted 2 branch(es)", r.stdout)
        self.assertEqual(sorted(s.heads), ["feature/other", "main"])
        self.assertEqual([s.pulls[n]["state"] for n in (1, 2, 3)], ["closed", "open", "closed"])

    def test_seed_adds_only_missing_files(self):
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(os.path.join(d, "base"))
            for name, text in (("base/a.yaml", "new\n"), ("base/b.yaml", "also new\n")):
                with open(os.path.join(d, name), "w") as f:
                    f.write(text)
            self.state.commit("main", "has a", {**self.state.commits[self.state.heads["main"]]["tree"], "kustomize/base/a.yaml": "mine\n"}, self.state.heads["main"])
            env = dict(os.environ, SCM="github", GITHUB_TOKEN=TOKEN, GITHUB_API=self.server.url, SCM_PROJECT="o/r", SCM_REF="main",
                       SCM_BACKOFF="0")
            r = subprocess.run([sys.executable, SCM_PY, "seed-local", d, "kustomize", "the Kustomize demo"], capture_output=True, text=True, env=env)
            self.assertEqual(r.returncode, 0, r.stderr)
            tree = self.state.commits[self.state.heads["main"]]["tree"]
            self.assertEqual((tree["kustomize/base/a.yaml"], tree["kustomize/base/b.yaml"]), ("mine\n", "also new\n"))
            r = subprocess.run([sys.executable, SCM_PY, "seed-local", d, "kustomize", "the Kustomize demo"], capture_output=True, text=True, env=env)
            self.assertIn("complete", r.stdout)


class FakeGitLab:
    def __init__(self):
        self.requests = []
        self.fail_next = []


class GitLabTests(unittest.TestCase):
    def test_the_call_goes_through_with_the_private_token(self):
        st = FakeGitLab()

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                st.requests.append((self.path, self.headers.get("PRIVATE-TOKEN")))
                if st.fail_next:
                    code = st.fail_next.pop(0)
                    self.send_response(code)
                    self.end_headers()
                    return
                body = b'{"iid": 4}'
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
        server = Server(H)
        self.addCleanup(server.close)
        st.fail_next = [502]
        code, out, err = run("gitlab", server.url, "GET", "/projects/o%2Fr/merge_requests/4")
        self.assertEqual((code, json.loads(out)), (0, {"iid": 4}))
        self.assertEqual(st.requests, [("/projects/o%2Fr/merge_requests/4", TOKEN)] * 2)
        self.assertNotIn(TOKEN, err)


if __name__ == "__main__":
    unittest.main()
