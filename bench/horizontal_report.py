#!/usr/bin/env python3
"""Markdown tables for docs/benchmarks.md from a bench/horizontal.sh results directory.

    python3 bench/horizontal_report.py bench/results/04-horizontal

Standard library only. Core groups follow bench/horizontal.sh's pinning: Postgres 0-2,
Redis + HAProxy 3, loadgen 4, instances 5-7.
"""
import csv
import json
import os
import re
import statistics
import sys
from collections import defaultdict

GROUPS = {"pg": {0, 1, 2}, "redis+lb": {3}, "loadgen": {4}, "instances": {5, 6, 7}}


def load_json(path):
    with open(path) as f:
        return json.load(f)


def load_ts(path):
    with open(path) as f:
        return [json.loads(line) for line in f if line.strip()]


def load_events(path):
    out = []
    if os.path.exists(path):
        with open(path) as f:
            for line in f:
                ts, _, what = line.strip().partition(" ")
                out.append((int(ts), what))
    return out


def core_busy(dirpath):
    """Mean busy % per core group per second: {ts: {group: pct}}."""
    path = os.path.join(dirpath, "cores.csv")
    per = defaultdict(lambda: defaultdict(list))
    if not os.path.exists(path):
        return {}
    with open(path) as f:
        for r in csv.DictReader(f):
            core = int(r["core"])
            for g, cores in GROUPS.items():
                if core in cores:
                    per[int(r["ts"])][g].append(float(r["busy_pct"]))
    return {ts: {g: statistics.mean(v) for g, v in gs.items()} for ts, gs in per.items()}


def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def rng(xs):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "–"
    m = statistics.median(xs)
    if min(xs) == max(xs):
        return f"{m:g}"
    return f"{m:g} ({min(xs):g}–{max(xs):g})"


def fmt_ms(x):
    return "–" if x is None else (f"{x:.0f}" if x >= 10 else f"{x:.1f}")


def step_at(summary, rate):
    for s in summary["steps"]:
        if s["rate"] == rate:
            return s
    return None


def size_runs(root):
    d = os.path.join(root, "sizes")
    runs = defaultdict(list)  # profile -> [summary]
    if os.path.isdir(d):
        for name in sorted(os.listdir(d)):
            m = re.match(r"(small|medium|large)-n(\d+)-r\d+\.summary\.json$", name)
            if m:
                runs[(m[1], int(m[2]))].append(load_json(os.path.join(d, name)))
    return runs


def reference_capacity(root):
    for (prof, _), rs in size_runs(root).items():
        if prof == "large":
            return med([s["capacity"] for s in rs])
    return None


def sizes(root):
    runs = size_runs(root)
    if not runs:
        return
    print("\nSame two instance cores, split three ways. Median (min–max) of the repetitions; latency at")
    print("each run's capacity step.\n")
    print("| profile | instances × CPU | capacity msg/s | send p50 / p99 ms | delivery p50 / p99 ms | lost / dup / reordered |")
    print("|---|---|---:|---:|---:|---:|")
    order = {"small": 0, "medium": 1, "large": 2}
    for (prof, n), rs in sorted(runs.items(), key=lambda kv: order[kv[0][0]]):
        cpu = {"small": "0.25", "medium": "1", "large": "2 (no quota)"}[prof]
        at = [step_at(s, s["capacity"]) for s in rs]
        at = [a for a in at if a]
        rel = [s["reliability"] for s in rs]
        print(f"| `{prof}` | {n} × {cpu} | {rng([s['capacity'] for s in rs])} | "
              f"{fmt_ms(med([a['send_p50_ms'] for a in at]))} / {fmt_ms(med([a['send_p99_ms'] for a in at]))} | "
              f"{fmt_ms(med([a['delivery_p50_ms'] for a in at]))} / {fmt_ms(med([a['delivery_p99_ms'] for a in at]))} | "
              f"{sum(r['lost'] for r in rel)} / {sum(r['redelivered'] + r['double_insert'] for r in rel)} / {sum(r['reorders'] for r in rel)} |")


def scale(root):
    d = os.path.join(root, "scale")
    if not os.path.isdir(d):
        return
    ref_cap = reference_capacity(root)
    runs = defaultdict(list)  # (wl, n) -> [(summary, cores)]
    for name in sorted(os.listdir(d)):
        m = re.match(r"(sync|send)-n(\d+)-r(\d+)\.summary\.json$", name)
        if m:
            base = os.path.join(d, name[: -len(".summary.json")])
            runs[(m[1], int(m[2]))].append((load_json(base + ".summary.json"), core_busy(base)))
    for wl in ("sync", "send"):
        ns = sorted(n for w, n in runs if w == wl)
        if not ns:
            continue
        caps = {n: [s["capacity"] for s, _ in runs[(wl, n)]] for n in ns}
        base_cap = med(caps[ns[0]]) or 1
        # Reference rate: the highest rate every N passed in every repetition.
        ref = min(min(c) for c in caps.values())
        print(f"\n#### {wl}-heavy\n")
        print(f"Latency columns at the reference rate {ref:g} msg/s (passed by every N). "
              "Capacity is median (min–max) of the repetitions. Busy % is the mean of the core "
              "group over the step at capacity.\n")
        print("| N | capacity msg/s | × N=1 | % of reference | send p50 / p99 ms | delivery p50 / p99 ms | Postgres busy | instances busy | lost / dup / reordered |")
        print("|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
        for n in ns:
            rs = runs[(wl, n)]
            refs = [step_at(s, ref) for s, _ in rs]
            refs = [r for r in refs if r]
            pg, inst = [], []
            for s, cores in rs:
                # Capacity step window: steps run back to back, each duration + settle long;
                # approximate it from the per-second series of that run is not available here,
                # so use the 10 busiest seconds as the saturated window.
                if cores:
                    top = sorted(cores.values(), key=lambda g: g.get("pg", 0), reverse=True)[:10]
                    pg.append(statistics.mean(g.get("pg", 0) for g in top))
                    inst.append(statistics.mean(g.get("instances", 0) for g in top))
            rel = [s["reliability"] for s, _ in rs]
            lost = sum(r["lost"] for r in rel)
            dup = sum(r["redelivered"] + r["double_insert"] for r in rel)
            reo = sum(r["reorders"] for r in rel)
            pct_ref = f"{100 * med(caps[n]) / ref_cap:.0f}%" if (ref_cap and wl == "sync") else "–"  # vs 1 large instance
            print(f"| {n} | {rng(caps[n])} | {med(caps[n]) / base_cap:.2f} | {pct_ref} | "
                  f"{fmt_ms(med([r['send_p50_ms'] for r in refs]))} / {fmt_ms(med([r['send_p99_ms'] for r in refs]))} | "
                  f"{fmt_ms(med([r['delivery_p50_ms'] for r in refs]))} / {fmt_ms(med([r['delivery_p99_ms'] for r in refs]))} | "
                  f"{fmt_ms(med(pg))}% | {fmt_ms(med(inst))}% | {lost} / {dup} / {reo} |")


def window(ts_rows, t0, t1):
    return [r for r in ts_rows if t0 <= r["ts"] < t1]


def around_events(name, base, events, before=5, after=30):
    ts_rows = load_ts(base + ".ts.jsonl")
    print(f"\n{name}:\n")
    print("| t (s) | event | send errors | retries | sync errors | reconnects | delivery p99 max ms |")
    print("|---:|---|---:|---:|---:|---:|---:|")
    start = ts_rows[0]["ts"] if ts_rows else 0
    for t, what in events:
        w = window(ts_rows, t - before, t + after)
        print(f"| {t - start} | {what} | {sum(r['send_err'] for r in w)} | {sum(r['retries'] for r in w)} | "
              f"{sum(r['sync_err'] for r in w)} | {sum(r['reconnects'] for r in w)} | "
              f"{fmt_ms(max((r['delivery_p99_ms'] for r in w), default=None))} |")


def summary_line(s, verify):
    r = s["reliability"]
    return (f"acked {r['acked']}, failed {r['failed']}, expected deliveries {r['expected']}, "
            f"lost {r['lost']}, late {r['delivered_late']}, unacked-but-delivered {r['unacked_delivered']}, "
            f"redelivered {r['redelivered']}, double-insert {r['double_insert']} (Postgres: {verify}), "
            f"reordered {r['reorders']}, sync errors {s['sync_errors']}, reconnects {s['reconnects']} "
            f"(max {fmt_ms(s['reconnect_max_ms'])} ms)")


def verify_dups(base):
    path = base + ".verify.txt"
    if not os.path.exists(path):
        return "?"
    with open(path) as f:
        text = f.read()
    m = re.search(r"^\s*(\d+)\s*\|\s*(\d+)\s*\|\s*(\d+)\s*$", text, re.M)
    return f"{m[3]} duplicates in {m[1]} rows" if m else "?"


def balance(base, events):
    """Seconds after each start until the new server takes >= 80% of a fair share of requests."""
    path = os.path.join(base, "haproxy.csv")
    if not os.path.exists(path):
        return {}
    per = defaultdict(dict)  # ts -> server -> stot
    status = defaultdict(dict)
    with open(path) as f:
        for r in csv.DictReader(f):
            if r["server"].startswith("n"):
                per[int(r["ts"])][r["server"]] = int(r["stot"])
                status[int(r["ts"])][r["server"]] = r["status"]
    ts = sorted(per)
    out = {}
    for t, what in events:
        m = re.search(r"start (\d+)", what)
        if not m:
            continue
        srv = "n" + m[1]
        for a, b in zip(ts, ts[1:]):
            if a < t:
                continue
            deltas = {s: per[b].get(s, 0) - per[a].get(s, 0) for s in per[b]}
            up = [s for s in deltas if status[b].get(s, "").startswith("UP") or status[b].get(s, "") == "DRAIN"]
            total = sum(deltas[s] for s in up)
            if srv in up and total and deltas[srv] >= 0.8 * total / len(up):
                out[what] = b - t
                break
    return out


def elastic(root):
    d = os.path.join(root, "elastic")
    if not os.path.isdir(d):
        return
    for mode in ("drain", "nodrain"):
        base = os.path.join(d, mode)
        if not os.path.exists(base + ".summary.json"):
            continue
        s = load_json(base + ".summary.json")
        events = load_events(base + ".events.txt")
        print(f"\n**{mode}** — {summary_line(s, verify_dups(base))}")
        around_events(f"Around each change ({mode})", base, events)
        bal = balance(base, events)
        if bal:
            print("\nNew instance at a fair share of requests after: " +
                  ", ".join(f"{k} {v}s" for k, v in bal.items()))


def redistrib(root):
    d = os.path.join(root, "redistrib")
    if not os.path.isdir(d):
        return
    print("\n| long-poll timeout | cycle | delivery max in the 30s after start (ms) | sync errors | reconnects | LB fair share after |")
    print("|---|---:|---:|---:|---:|---:|")
    for to in ("30s", "5s"):
        base = os.path.join(d, "timeout" + to)
        if not os.path.exists(base + ".summary.json"):
            continue
        ts_rows = load_ts(base + ".ts.jsonl")
        events = load_events(base + ".events.txt")
        bal = balance(base, events)
        for t, what in events:
            if "start" not in what:
                continue
            w = window(ts_rows, t, t + 30)
            cyc = re.search(r"cycle (\d+)", what)
            print(f"| {to} | {cyc[1] if cyc else '?'} | {fmt_ms(max((r['delivery_max_ms'] for r in w), default=None))} | "
                  f"{sum(r['sync_err'] for r in w)} | {sum(r['reconnects'] for r in w)} | {bal.get(what, '–')}s |")
        s = load_json(base + ".summary.json")
        print(f"\ntimeout {to}: {summary_line(s, verify_dups(base))}\n")


def recovery(ts_rows, t0, slo_ms=500, quiet=5):
    """Seconds from t0 until `quiet` consecutive seconds with no errors and p99 under slo."""
    w = [r for r in ts_rows if r["ts"] >= t0]
    run = 0
    for r in w:
        ok = r["send_err"] == 0 and r["sync_err"] == 0 and r["delivery_p99_ms"] < slo_ms
        run = run + 1 if ok else 0
        if run == quiet:
            return r["ts"] - quiet + 1 - t0
    return None


def faults(root):
    d = os.path.join(root, "faults")
    if not os.path.isdir(d):
        return
    print("\n| fault | failed sends | retries | lost | late | dup (client / Postgres) | reordered | sync errors | reconnect max ms | delivery max ms | recovered after |")
    print("|---|---:|---:|---:|---:|---|---:|---:|---:|---:|---:|")
    for fault in ("kill-instance", "pause-redis", "restart-redis", "restart-postgres", "boot-without-redis"):
        base = os.path.join(d, fault)
        if not os.path.exists(base + ".summary.json"):
            continue
        s = load_json(base + ".summary.json")
        r = s["reliability"]
        ts_rows = load_ts(base + ".ts.jsonl")
        ev = dict((w, t) for t, w in load_events(base + ".events.txt"))
        t0 = ev.get(f"{fault} begin")
        t1 = ev.get(f"{fault} end", t0)
        w = window(ts_rows, t0, t1 + 40) if t0 else ts_rows
        rec = recovery(ts_rows, t1) if t1 else None
        retries = sum(st["retries"] for st in s["steps"])
        print(f"| {fault} | {r['failed']} | {retries} | {r['lost']} | {r['delivered_late']} | "
              f"{r['redelivered'] + r['double_insert']} / {verify_dups(base)} | {r['reorders']} | {s['sync_errors']} | "
              f"{fmt_ms(s['reconnect_max_ms'])} | {fmt_ms(max((x['delivery_max_ms'] for x in w), default=None))} | "
              f"{'–' if rec is None else f'{rec}s'} |")


def main():
    root = sys.argv[1]
    for title, fn in (("Profiles on the same hardware", sizes), ("Scalability", scale), ("Elasticity", elastic),
                      ("Redistribution and reconnection", redistrib), ("Failures", faults)):
        print(f"\n### {title}")
        fn(root)


if __name__ == "__main__":
    main()
