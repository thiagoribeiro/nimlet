"""Memory table of a bench/syncramp.sh run.

    python3 bench/syncramp_report.py bench/results/<name>

Per loadgen run: means over its measured seconds (the .ts.jsonl range), then over the idle
gap after it; and a least-squares fit of instance memory against open long-polls.
"""
import csv, json, sys, collections, statistics
MB = 1 << 20
d = sys.argv[1]
rows = list(csv.DictReader(open(f'{d}/mem.csv')))
by = collections.defaultdict(list)
for r in rows:
    by[r['name']].append((int(r['ts']), r))
ev = [l.split(None, 2) for l in open(f'{d}/events.txt')]
starts = {e[2].strip(): int(e[0]) for e in ev if len(e) > 2 and e[1] == 'start'}
ends = {e[2].strip(): int(e[0]) for e in ev if len(e) > 2 and e[1] == 'end'}
order = sorted(starts, key=starts.get)


def mean(name, k, t0, t1):
    v = [float(r[k]) for ts, r in by[name] if t0 <= ts <= t1 and r.get(k)]
    return statistics.mean(v) if v else None


fmt = lambda v, s=MB: '-' if v is None else f'{v/s:.0f}'
inst, lb, rd, pg = 'nimlet-bench-n1', 'nimlet-bench-lb', 'nimlet-bench-redis', 'nimlet-bench-pg'
print('| run | long-polls | window | instance anon MB | Go heap MB | Go sys MB | goroutines '
      '| HAProxy MB | Redis MB | PG anon MB | loadgen RSS MB | delivery p50/p99 ms | sync/s |')
print('|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|')
fit = []
idle0 = min(starts.values())
for i, base in enumerate(order):
    n = int(base[1:].split('-')[0])
    secs = [json.loads(l) for l in open(f'{d}/{base}.ts.jsonl')]
    t0, t1 = secs[0]['ts'], secs[-1]['ts']
    p50 = statistics.median(s['delivery_p50_ms'] for s in secs)
    p99 = statistics.median(s['delivery_p99_ms'] for s in secs)
    syn = statistics.mean(s['sync_ok'] for s in secs)
    nxt = starts[order[i + 1]] if i + 1 < len(order) else ends[base] + 20
    wins = [('load', t0, t1, f'{p50:.0f}/{p99:.0f}', f'{syn:.0f}'),
            ('idle after', ends[base] + 5, nxt - 1, '', '')]
    if i == 0:
        wins.insert(0, ('idle before', idle0 - 20, idle0 - 1, '', ''))
    for lab, a, b, lat, s in wins:
        an = mean(inst, 'anon', a, b)
        if lab == 'load' and an:
            fit.append((n, an))
        print(f'| {base} | {n if lab == "load" else 0} | {lab} | {fmt(an)} | {fmt(mean(inst, "heap_inuse", a, b))} '
              f'| {fmt(mean(inst, "go_sys", a, b))} | {fmt(mean(inst, "goroutines", a, b), 1)} '
              f'| {fmt(mean(lb, "current", a, b))} | {fmt(mean(rd, "current", a, b))} | {fmt(mean(pg, "anon", a, b))} '
              f'| {fmt(mean("loadgen", "current", a, b))} | {lat} | {s} |')
if len(fit) > 1:
    xs, ys = zip(*fit)
    slope, icpt = statistics.linear_regression(xs, ys)
    print(f'\ninstance anon ≈ {icpt/MB:.0f} MB + {slope/1024:.1f} KB per open long-poll (fit over {len(fit)} runs)')
peak = max((float(r['anon']), ts) for ts, r in by[inst] if r.get('anon'))
print(f'instance anon peak {peak[0]/MB:.0f} MB')
