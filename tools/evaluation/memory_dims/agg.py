"""Summarise memory_dims sweep runs (run from the OUT folder: reads runs/<set>.<single|span>.d<dims>[.mv].jsonl).
Prints counts only: R@5, R@1, candidate-pool hits, abstentions. No question text."""
import json,glob,re,collections,statistics,sys,os

# evaluation sets: the data root names under SRC (run_sweep.sh SETS)
SETS=os.environ.get('SETS','discord kakao-a kakao-b').split()
rows=collections.defaultdict(dict)
for p in sorted(glob.glob('runs/*.jsonl')):
    m=re.match(r'runs/(\w+)\.(single|span)\.d(\d+)(\.mv)?\.jsonl',p.replace('\\','/'))  # Windows glob gives runs\name
    if not m: continue
    st,g,d=m.group(1),m.group(2),m.group(3)+(m.group(4) or '')
    qs=[json.loads(l) for l in open(p,encoding='utf-8')]
    for mode in ('hybrid','rerank'):
        ans=[q for q in qs if not q.get('expected_abstention') and not q.get('anchor_missing')]
        ab=[q for q in qs if q.get('expected_abstention')]
        rk=[q['modes'][mode].get('rank',0) for q in ans]
        r1=sum(1 for r in rk if r==1); r5=sum(1 for r in rk if 0<r<=5)
        mrr=sum(1/r for r in rk if r)/len(ans)
        pool=sum(1 for q in ans if q['modes'][mode].get('pool_hit'))
        abst=sum(1 for q in ab if q['modes'][mode].get('abstained') or q['modes'][mode].get('n_hits',0)==0)
        ms=statistics.median(q['modes'][mode].get('ms',0) for q in qs)
        rows[(st,g,mode)][d]=(r1,r5,mrr,pool,abst,len(ans),ms)
dims=sorted({d for v in rows.values() for d in v},key=lambda x:(-int(x.split('.')[0]),x))
for g in ('single','span'):
  for mode in ('hybrid','rerank'):
    print(f"\n## {g} / {'R0' if mode=='hybrid' else 'R2'}   R@5 (R@1) [pool] — answerable n")
    print('set'.ljust(9)+''.join(str(d).rjust(16) for d in dims))
    tot=collections.defaultdict(lambda:[0,0,0,0])
    for st in SETS:
        v=rows.get((st,g,mode),{})
        line=st.ljust(9)
        for d in dims:
            if d in v:
                r1,r5,mrr,pool,abst,n,ms=v[d]; line+=f"{r5:>5}({r1:>2})[{pool:>2}]/{n}".rjust(16)
                t=tot[d]; t[0]+=r5; t[1]+=r1; t[2]+=n; t[3]+=pool
            else: line+='-'.rjust(16)
        print(line)
    print('total'.ljust(9)+''.join((f"{tot[d][0]}({tot[d][1]})[{tot[d][3]}]/{tot[d][2]}" if tot[d][2] else '-').rjust(16) for d in dims))
print("\n## abstain (R2, unanswerable correctly emptied /10) and p50 ms")
for st in SETS:
    v=rows.get((st,'single','rerank'),{}); h=rows.get((st,'single','hybrid'),{})
    print(st.ljust(9)+''.join((f"{v[d][4]}/10 {h[d][6]:.0f}ms" if d in v else '-').rjust(16) for d in dims))
