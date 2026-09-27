import json
import pathlib
import shlex
import shutil
import sys


def prepare(path):
    bazel = shutil.which("bazel")
    if not bazel:
        raise ValueError("Bazel is unavailable")
    path.write_text("#!/bin/sh\n" +
                    "if [ \"${1-}\" = test ]; then\n" +
                    "  shift\n" +
                    "  exec " + shlex.quote(bazel) + " test --nocache_test_results \"$@\"\n" +
                    "fi\nexec " + shlex.quote(bazel) + " \"$@\"\n")
    path.chmod(0o755)


def verify(path):
    events = [json.loads(line) for line in (path / "events.jsonl").read_text().splitlines()]
    configured = {event["id"]["targetConfigured"]["label"] for event in events
                  if event.get("configured", {}).get("testSize")}
    summaries = {event["id"]["testSummary"]["label"]: event["testSummary"] for event in events
                 if "testSummary" in event}
    results = [event["testResult"] for event in events if "testResult" in event]
    options = {option["optionName"]: option.get("optionValue", "")
               for event in events if event.get("structuredCommandLine", {}).get("commandLineLabel") == "canonical"
               for section in event["structuredCommandLine"]["sections"]
               for option in section.get("optionList", {}).get("option", [])}
    report = json.loads((path / "report.json").read_text())
    ledger = json.loads((path / "check-ledger/infra-fast.json").read_text())
    passed = (bool(configured) and configured == summaries.keys() and bool(results)
              and sum(summary.get("totalRunCount", 0) for summary in summaries.values()) == len(results)
              and all(result.get("status") == "PASSED" and not result.get("cachedLocally")
                      and not result.get("executionInfo", {}).get("cachedRemotely") for result in results)
              and all(summary.get("overallStatus") == "PASSED" and not summary.get("totalNumCached")
                      for summary in summaries.values())
              and options.get("cache_test_results") in ("no", "false", "0")
              and options.get("remote_timeout") == "10s" and options.get("jobs") == "100"
              and options.get("remote_cache") == "grpc://100.87.168.66:9093"
              and report.get("remote_cache") == "used" and report.get("success")
              and ledger.get("success") and not ledger.get("budget_exceeded")
              and ledger.get("budget_seconds") == 10)
    receipt = {"schema": 1, "targets": sorted(configured), "test_results": len(results),
               "cached_results": sum(bool(result.get("cachedLocally") or result.get("executionInfo", {}).get("cachedRemotely"))
                                     for result in results),
               "duration_seconds": ledger.get("duration_seconds"), "passed": passed}
    (path / "cold-cache-qualification.json").write_text(json.dumps(receipt, indent=2) + "\n")
    print(json.dumps(receipt))
    if not passed:
        raise ValueError("Hosted cold-test qualification failed")


if __name__ == "__main__":
    {"prepare": prepare, "verify": verify}[sys.argv[1]](pathlib.Path(sys.argv[2]))
