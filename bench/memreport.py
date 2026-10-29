"""Memory tables of a bench/horizontal.sh sizes run sampled by bench/memsample.sh.

    python3 bench/memreport.py MEM.csv bench/results/<name>

Per profile: start of the run, mean over the seconds at the capacity step, and peak.
"""
import csv, json, sys, collections
MB = 1 << 20
rows = list(csv.DictReader(open(sys.argv[1])))
d = sys.argv[2]
for prof in ['large-n1-r1', 'medium-n2-r1', 'small-n8-r1']:
    secs = [json.loads(l) for l in open(f'{d}/sizes/{prof}.ts.jsonl')]
    t0, t1 = secs[0]['ts'], secs[-1]['ts']
    cap = json.load(open(f'{d}/sizes/{prof}.summary.json'))['capacity']
    capts = {r['ts'] for r in secs if abs(r['send_ok'] - cap) <= cap * 0.03}
    print(f'\n## {prof}  capacity {cap} msg/s  ({t1-t0}s run, {len(capts)}s at capacity)')
    by = collections.defaultdict(list)
    for r in rows:
        ts = int(r['ts'])
        if t0 - 5 <= ts <= t1 + 2:
            by[r['name']].append((ts, r))
    inst = sorted(n for n in by if n.startswith('nimlet-bench-n') and n != 'nimlet-bench-n9')
    def f(r, k):
        v = r.get(k) or ''
        return float(v) if v else None
    def stats(name, k):
        all_ = [f(r, k) for ts, r in by[name] if f(r, k) is not None]
        at = [f(r, k) for ts, r in by[name] if ts in capts and f(r, k) is not None]
        start = all_[0] if all_ else None
        return start, (sum(at) / len(at) if at else None), (max(all_) if all_ else None)
    fmt = lambda v, s=MB: '-' if v is None else f'{v/s:.0f}'
    print('| component | metric | start | mean at capacity | peak |')
    for n in inst[:2] + (['…'] if len(inst) > 2 else []):
        if n == '…': continue
        for k, lab in [('current', 'cgroup MB'), ('anon', 'anon MB'), ('heap_inuse', 'Go heap in use MB'), ('go_sys', 'Go sys MB')]:
            a, b, c = stats(n, k); print(f'| {n} | {lab} | {fmt(a)} | {fmt(b)} | {fmt(c)} |')
        a, b, c = stats(n, 'goroutines'); print(f'| {n} | goroutines | {fmt(a,1)} | {fmt(b,1)} | {fmt(c,1)} |')
    if len(inst) > 1:
        # sum across instances per sample time
        tot = collections.defaultdict(float)
        for n in inst:
            for ts, r in by[n]:
                tot[ts] += float(r['current'])
        vals = list(tot.values()); at = [v for ts, v in tot.items() if ts in capts]
        print(f'| all {len(inst)} instances | cgroup MB total | - | {fmt(sum(at)/len(at) if at else None)} | {fmt(max(vals))} |')
    for n in ['nimlet-bench-pg', 'nimlet-bench-redis', 'nimlet-bench-lb', 'loadgen', 'pg-datadir']:
        for k, lab in ([('current', 'cgroup MB'), ('anon', 'anon MB'), ('shmem', 'shmem MB')] if n == 'nimlet-bench-pg' else [('current', 'RSS MB' if n == 'loadgen' else ('bytes MB' if n == 'pg-datadir' else 'cgroup MB'))]):
            a, b, c = stats(n, k); print(f'| {n} | {lab} | {fmt(a)} | {fmt(b)} | {fmt(c)} |')
