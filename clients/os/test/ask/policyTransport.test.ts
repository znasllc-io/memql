import {expect,it,vi,afterEach} from 'vitest';
import {QueryClient,Result} from '@znasllc-io/memql-sdk-core/client';
import {SdkAskTransport,isPolicyAuthoringRequest} from '../../src/ask/sdkTransport';
afterEach(()=>vi.restoreAllMocks());
it('routes an explicit policy request to the draft compiler and never saves before review',async()=>{
 const describe=vi.spyOn(QueryClient.prototype,'routingPolicyDescribe').mockResolvedValue(new Result({data:[{name:'mine',action:'save',primary:'fleet:fastest',fallbacks:['app:*'],revision:4,description:'Local first'}]} as never));
 const save=vi.spyOn(QueryClient.prototype,'routingPolicySave');const stream=vi.fn();const proposal=vi.fn();
 await new Promise<void>((resolve,reject)=>new SdkAskTransport(()=>({} as any),stream).ask('Create a policy for local work',null,{delta:()=>{},policyProposal:proposal,done:resolve,error:reject}));
 expect(describe).toHaveBeenCalledWith({sentence:'Create a policy for local work'},expect.objectContaining({signal:expect.any(AbortSignal)}));expect(proposal).toHaveBeenCalledWith(expect.objectContaining({primary:'fleet:fastest',revision:4}));expect(save).not.toHaveBeenCalled();expect(stream).not.toHaveBeenCalled();
});
it('keeps ordinary questions in chat while explicit composition accepts natural wording',()=>{
 expect(isPolicyAuthoringRequest('What is a routing policy?',null)).toBe(false);
 expect(isPolicyAuthoringRequest('Try the fastest local model, then an app','action:routing-policy')).toBe(true);
});
