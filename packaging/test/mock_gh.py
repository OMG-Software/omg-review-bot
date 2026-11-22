#!/usr/bin/env python3
"""A fake gh covering the calls publish-latest.sh makes, backed by a JSON state file.

State: tag (sha or null), release (null or {id,title,body,assets:[...]}),
compare ({"A...B": status}), fail ({"upload": n, "rename": n, "tagpatch": true}),
counts, next_id. Prints what the script's --jq expression would.
"""
import hashlib, json, os, sys

path = os.environ["MOCK_STATE"]
st = json.load(open(path))
args = sys.argv[1:]


def save():
    json.dump(st, open(path, "w"), indent=1)


def die(msg, code=1):
    save()
    print(msg, file=sys.stderr)
    sys.exit(code)


def count(kind):
    st["counts"][kind] = st["counts"].get(kind, 0) + 1
    return st["counts"][kind]


def opts(rest):
    out, pos, i = {}, [], 0
    while i < len(rest):
        a = rest[i]
        if a in ("-X", "--jq", "--json", "--title", "--notes-file"):
            out[a] = rest[i + 1]; i += 2
        elif a in ("-f", "-F"):
            k, v = rest[i + 1].split("=", 1); out.setdefault("fields", {})[k] = v; i += 2
        elif a.startswith("--"):
            out[a] = True; i += 1
        else:
            pos.append(a); i += 1
    return out, pos


rel = st["release"]
if args[0] == "api":
    o, pos = opts(args[1:])
    ep = pos[0].split("?")[0].split("/", 3)[3]  # strip repos/OWNER/NAME/
    method = o.get("-X", "POST" if "fields" in o else "GET")
    f = o.get("fields", {})
    if ep == "git/ref/tags/latest":
        if not st["tag"]:
            die("Not Found (HTTP 404)")
        print(st["tag"])
    elif ep.startswith("compare/"):
        print(st["compare"].get(ep[len("compare/"):], "ahead"))
    elif ep == "git/refs" and method == "POST":
        if st["tag"]:
            die("Reference already exists (HTTP 422)")
        st["tag"] = f["sha"]
    elif ep == "git/refs/tags/latest" and method == "PATCH":
        away = f["sha"] != st["tag_original"]
        if st["fail"].get("tagpatch") and away:
            die("tag patch failed (HTTP 500)")
        st["tag"] = f["sha"]
        if st["fail"].get("tagpatch_lost") and away:
            die("tag patch applied, response lost (HTTP 502)")
    elif ep == "git/refs/tags/latest" and method == "DELETE":
        if not st["tag"]:
            die("Reference does not exist (HTTP 422)")
        st["tag"] = None
    elif ep == "releases/tags/latest":
        if not rel:
            die("Not Found (HTTP 404)")
        print(rel["id"])
    elif ep.endswith("/assets") and method == "GET":
        if count("list") >= st["fail"].get("list_from", 1 << 30):
            die("listing failed (HTTP 500)")
        for a in rel["assets"]:
            print(f'{a["id"]}\t{a["name"]}\t{a["size"]}\t{a["state"]}')
    elif ep.startswith("releases/assets/"):
        aid = int(ep.rsplit("/", 1)[1])
        a = next((x for x in rel["assets"] if x["id"] == aid), None)
        if a is None:
            die("asset not found (HTTP 404)")
        if method == "PATCH":
            n = count("rename")

            def hit(key):
                v = st["fail"].get(key)
                return n == v or (isinstance(v, list) and n in v)
            # rename: fails before applying; rename_from: every rename from
            # that one on fails; rename_lost: applied, then reports failure.
            if hit("rename") or n >= st["fail"].get("rename_from", 1 << 30):
                die("rename failed (HTTP 500)")
            if any(x["name"] == f["name"] for x in rel["assets"]):
                die("name taken (HTTP 422)")
            a["name"] = f["name"]
            if hit("rename_lost"):
                die("rename applied, response lost (HTTP 502)")
        elif method == "DELETE":
            rel["assets"].remove(a)
    else:
        die(f"mock: unhandled api {method} {ep}", 3)
elif args[0] == "release":
    o, pos = opts(args[2:])
    sub = args[1]
    if sub == "view":
        if not rel:
            die("release not found")
        print(rel["name"] if o.get("--jq") == ".name" else rel["body"])
    elif sub == "create":
        if rel:
            die("already exists")
        if not st["tag"]:
            die("tag doesn't exist")
        st["release"] = {"id": 900, "name": o["--title"], "body": open(o["--notes-file"]).read(), "assets": []}
    elif sub == "delete":
        if not rel:
            die("release not found")
        if st["fail"].get("release_delete"):
            die("delete failed (HTTP 500)")
        st["release"] = None
    elif sub == "edit":
        rel["name"] = o["--title"]
        rel["body"] = open(o["--notes-file"]).read()
    elif sub == "upload":
        for fp in pos[1:]:
            n = count("upload")
            if st["fail"].get("upload") == n:
                die("upload failed")
            name = os.path.basename(fp)
            if any(x["name"] == name for x in rel["assets"]):
                die("asset exists")
            data = open(fp, "rb").read()
            st["next_id"] += 1
            rel["assets"].append({"id": st["next_id"], "name": name, "size": len(data), "state": "uploaded",
                                  "sha": hashlib.sha256(data).hexdigest()})
    else:
        die(f"mock: unhandled release {sub}", 3)
else:
    die(f"mock: unhandled {args}", 3)
save()
