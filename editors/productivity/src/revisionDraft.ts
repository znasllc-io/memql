/** Decode only public replacement strings from an incomplete JSON proposal.
 * Never render raw JSON, reasoning, prompts, or a guessed repaired proposal.
 * Incomplete escape sequences are withheld until their remaining bytes arrive.
 */
export function revisionDraftParts(raw: unknown): {before:string;after:string;complete:boolean}[] {
 if(typeof raw!=="string" || raw.length>2*1024*1024)return [];
 const parts:{before:string;after:string;complete:boolean}[]=[];
 let field="",before="";
 for(let i=0;i<raw.length;i++){
  if(raw[i]!== '"')continue;
  const start=++i;let escaped=false;
  for(;i<raw.length;i++){if(escaped){escaped=false;continue;}if(raw[i]==='\\'){escaped=true;continue;}if(raw[i]==='"')break;}
  const complete=i<raw.length;let encoded=raw.slice(start,i),value:string|undefined;
  for(let trim=0;trim<7 && trim<=encoded.length;trim++){
   try{value=JSON.parse('"'+encoded.slice(0,encoded.length-trim)+'"');break;}catch{if(complete)break;}
  }
  if(value===undefined)break;
  // Do not display half of a UTF-16 surrogate pair from an unfinished escape.
  if(!complete && /[\uD800-\uDBFF]$/.test(value))value=value.slice(0,-1);
  let next=i+1;while(/\s/.test(raw[next]??"")&&next<raw.length)next++;
  if(complete && raw[next]===":"){field=value;i=next;continue;}
  if(field==="before")before=value;
  if(field==="after"&&value){parts.push({before,after:value,complete});before="";}
  field="";
 }
 return parts.slice(0,100);
}
