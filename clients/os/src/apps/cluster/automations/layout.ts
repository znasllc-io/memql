export interface Edge { to: string; concept?: string; topic?: string; decided: boolean; reason: string }
export interface Automation {
  name: string; stratum: number; origin?: string; trigger?: string; triggerKind?: string;
  triggerConcept?: string; schedule?: string; triggerFilter?: string; writes: string[];
  publishes: string[]; opaque: string[]; edgesOut: Edge[];
  loop?: { maxDepth: number; until: string }; mode?: { kind: string; max?: number };
  cycle?: { members: string[]; permittedBy: string[]; permitted: boolean }; problem?: string;
}
export const compare = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0;
export const short = (value: string) => value.split(":").at(-1) ?? value;
export function trigger(a: Automation): string {
  if (a.schedule) return `schedule ${a.schedule}`;
  if (a.triggerConcept) return `${a.triggerKind === "updated" ? "an update to" : a.triggerKind?.startsWith("before") ? a.triggerKind.replaceAll("_", " ").replaceAll(".", " ") + " of" : "any write to"} ${short(a.triggerConcept)}`;
  return a.trigger ? `topic ${a.trigger}` : "Called directly";
}
export interface Card { row: Automation; x: number; y: number; width: number; height: number }
export interface Curve { from: string; to: string; labels: string[]; reasons: string[]; decided: boolean; path: string; x: number; y: number; start: [number, number]; end: [number, number] }
/** Content coordinates only: stable across wire ordering and independent of the DOM. */
export function layout(rows: Automation[]) {
  const sorted = [...rows].sort((a, b) => a.stratum - b.stratum || compare(a.name, b.name));
  const names = new Set(rows.map(a => a.name));
  const connected = new Set<string>();
  for (const a of sorted) for (const e of a.edgesOut) if (names.has(e.to)) { connected.add(a.name); connected.add(e.to); }
  const columns = [...new Set(sorted.filter(a => connected.has(a.name)).map(a => a.stratum))];
  const cards: Card[] = [];
  for (const [i, stratum] of columns.entries()) {
    const groupKey = (a: Automation) => a.cycle ? [...a.cycle.members].sort(compare).join("|") : "~";
    const barycenter = (a: Automation) => {
      const prior = cards.filter(c => c.row.edgesOut.some(e => e.to === a.name));
      return prior.length ? prior.reduce((sum, c) => sum + c.y, 0) / prior.length : 0;
    };
    const column = sorted.filter(a => a.stratum === stratum && connected.has(a.name)).sort((a,b) => compare(groupKey(a), groupKey(b)) || barycenter(a)-barycenter(b) || compare(a.name,b.name));
    column.forEach((row,j) => cards.push({row,x:24+i*308,y:88+j*116,width:196,height:56}));
  }
  const byName = new Map(cards.map(c => [c.row.name,c]));
  const edges: Curve[] = [];
  for (const card of cards) {
    const pairs = new Map<string, Edge[]>();
    for (const e of card.row.edgesOut) if (byName.has(e.to)) pairs.set(e.to,[...(pairs.get(e.to) ?? []),e]);
    for (const [to, pair] of [...pairs].sort(([a],[b]) => compare(a,b))) {
      pair.sort((a,b) => compare(a.concept || a.topic || "", b.concept || b.topic || ""));
      const target = byName.get(to)!;
      const sx=card.x+card.width, sy=card.y+28, tx=target.x, ty=target.y+28;
      const same = card.x === target.x;
      const self = card === target;
      const start: [number,number] = [sx,sy];
      const end: [number,number] = self ? [sx,card.y+44] : [tx,ty];
      const path = self ? `M ${sx} ${sy} C ${sx+52} ${sy-30}, ${sx+52} ${sy+46}, ${end[0]} ${end[1]}` : same ? `M ${sx} ${sy} C ${sx+58} ${sy}, ${tx-20} ${ty+68}, ${tx} ${ty}` : `M ${sx} ${sy} C ${(sx+tx)/2} ${sy}, ${(sx+tx)/2} ${ty}, ${tx} ${ty}`;
      edges.push({from:card.row.name,to,labels:pair.map(e => short(e.concept || e.topic || "write")),reasons:pair.map(e=>e.reason),decided:pair.every(e=>e.decided),path,x:self?sx+24:(sx+tx)/2,y:self?sy+56:same?(sy+ty)/2+30:(sy+ty)/2-8,start,end});
    }
  }
  const groups = [...new Set(cards.filter(c=>c.row.cycle).map(c=>[...c.row.cycle!.members].sort(compare).join("|")))].map(key => {
    const members=cards.filter(c=>c.row.cycle && [...c.row.cycle.members].sort(compare).join("|")===key);
    const x=Math.min(...members.map(c=>c.x))-10,y=Math.min(...members.map(c=>c.y))-10;
    return {key,members,x,y,width:Math.max(...members.map(c=>c.x+c.width))-x+10,height:Math.max(...members.map(c=>c.y+c.height))-y+48,permitted:members[0]!.row.cycle!.permitted};
  });
  return {columns,cards,edges,groups,standalone:sorted.filter(a=>!connected.has(a.name)),width:Math.max(300,columns.length*308),height:Math.max(220,...cards.map(c=>c.y+112))};
}
