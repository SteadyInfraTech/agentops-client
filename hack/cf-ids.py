#!/usr/bin/env python3
"""Read-only Cloudflare lookups for hack/kind-smoke.sh (LIVE mode). Standard library only.

  cf-ids.py dns            print IDs of DNS records named $HOST in zone $CLOUDFLARE_ZONE_ID
  cf-ids.py access         print IDs of Access applications whose domain is $HOST (follows pagination)
  cf-ids.py tunnel ID      print "deleted", or "exists"; a Cloudflare API error is a failure

Any API error, non-success response or unexpected shape exits non-zero with a message on stderr, so a
caller can never mistake an error for "nothing there".
"""
import json
import os
import sys
import urllib.error
import urllib.request

API = "https://api.cloudflare.com/client/v4/"


def call(path):
    req = urllib.request.Request(API + path, headers={"Authorization": "Bearer " + os.environ["CLOUDFLARE_API_TOKEN"]})
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            body = json.load(resp)
    except urllib.error.HTTPError as e:
        try:
            body = json.load(e)
        except Exception:
            sys.exit("cloudflare %s: HTTP %d" % (path, e.code))
        body["_http"] = e.code
    except Exception as e:  # network errors, bad JSON
        sys.exit("cloudflare %s: %s" % (path, e))
    return body


def paged(path):
    page, out = 1, []
    while True:
        body = call("%s%sper_page=100&page=%d" % (path, "&" if "?" in path else "?", page))
        if body.get("success") is not True or not isinstance(body.get("result"), list):
            sys.exit("cloudflare %s: %s" % (path, json.dumps(body.get("errors", body))[:300]))
        out += body["result"]
        if page >= (body.get("result_info") or {}).get("total_pages", 1):
            return out
        page += 1


def main():
    what = sys.argv[1]
    if what == "dns":
        for r in paged("zones/%s/dns_records?name=%s" % (os.environ["CLOUDFLARE_ZONE_ID"], os.environ["HOST"])):
            print(r["id"])
    elif what == "access":
        for a in paged("accounts/%s/access/apps" % os.environ["CLOUDFLARE_ACCOUNT_ID"]):
            if a.get("domain") == os.environ["HOST"]:
                print(a["id"])
    elif what == "tunnel":
        body = call("accounts/%s/cfd_tunnel/%s" % (os.environ["CLOUDFLARE_ACCOUNT_ID"], sys.argv[2]))
        if body.get("success") is True:
            print("deleted" if (body.get("result") or {}).get("deleted_at") else "exists")
        elif any("not found" in str(e.get("message", "")).lower() for e in body.get("errors", [])):
            print("deleted")
        else:
            sys.exit("cloudflare tunnel lookup: %s" % json.dumps(body.get("errors", body))[:300])
    else:
        sys.exit("usage: cf-ids.py dns|access|tunnel ID")


main()
