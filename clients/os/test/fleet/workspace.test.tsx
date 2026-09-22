import {act, cleanup, fireEvent, render, screen} from '@testing-library/react';
import {afterEach, beforeEach, expect, it, vi} from 'vitest';
const h=vi.hoisted(()=>({connection:null as any}));
vi.mock('../../src/live/connection',()=>({useOsConnection:()=>h.connection,osBridgePath:'',bridgePathFor:()=>''}));
const {fakeConnection,machineRow,withSession,rowsResult}=await import('./harness');
const {FleetApp}=await import('../../src/apps/fleet/FleetApp');
const {MachinesProvider,WORKER_REGISTRATION_CONCEPT}=await import('../../src/live/machines');
const {PolicyEditor}=await import('../../src/apps/fleet/PolicyEditor');
const {ActivityTarget,SemanticActivityProvider}=await import('../../src/kit/SemanticActivity');
const {installSeededAccess}=await import('../seededAccess');
afterEach(cleanup);
beforeEach(()=>{installSeededAccess('owner');h.connection=fakeConnection();});
// A machine is opened from the LIST: the section no longer selects the first one
// on sight, and there is no dropdown to pick another from.
const openMachine=async(name:string)=>fireEvent.click(await screen.findByRole('button',{name:new RegExp(`^Open ${name}`)}));
function fleet(){return render(withSession(<MachinesProvider><FleetApp sectionId="machines" navigate={vi.fn()} askContext={vi.fn()} store={{load:()=>({version:1,defaultSection:'machines',showRevoked:false}),save:()=>{}}}/></MachinesProvider>));}
it('returns to the list when the selected machine disappears, never to another machine',async()=>{
 h.connection=fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'}),machineRow({id:'b',displayName:'Beta'})]});fleet();
 await openMachine('Beta');
 fireEvent.click(await screen.findByRole('button',{name:/Machine details/}));expect(screen.getByLabelText('Name for Beta')).toBeTruthy();
 await act(async()=>h.connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT,machineRow({id:'b',displayName:'Beta',revokedAt:new Date().toISOString()}),'NODE_UPDATED'));
 expect(await screen.findByRole('button',{name:/^Open Alpha/})).toBeTruthy();expect(screen.queryByRole('navigation',{name:'Machine views'})).toBeNull();expect(screen.queryByLabelText('Name for Alpha')).toBeNull();expect(screen.queryByText('That machine is no longer in this view')).toBeNull();
});
it('saves exactly the manually ordered chain and inspected revision, retaining a refused draft',async()=>{
 const save=vi.fn().mockRejectedValueOnce(new Error('stale revision')).mockResolvedValue(rowsResult([]));h.connection.query.routingPolicySave=save;
 render(withSession(<PolicyEditor seed={{name:'mine',description:'Local first',primary:'fleet:strongest',fallbacks:['app:*'],revision:7}}/>));
 fireEvent.click(screen.getByRole('button',{name:'Move source 2 earlier'}));fireEvent.click(screen.getByRole('button',{name:'Save policy'}));
 expect(await screen.findByText('stale revision')).toBeTruthy();expect((screen.getByLabelText('Policy name') as HTMLInputElement).value).toBe('mine');
 expect(save).toHaveBeenCalledWith({name:'mine',description:'Local first',primary:'app:*',fallbacks:['fleet:strongest'],expectedRevision:7});
});
it('has no AI activity by default and never changes keyboard focus as activity updates',()=>{
 const view=render(<SemanticActivityProvider value={[]}><button>Manual control</button><ActivityTarget target="machine:a"><span>Equipment</span></ActivityTarget></SemanticActivityProvider>);
 const button=screen.getByRole('button');button.focus();expect(screen.queryByRole('status')).toBeNull();
 view.rerender(<SemanticActivityProvider value={[{id:'event-1',target:'machine:a',phase:'running',label:'Reading capabilities'}]}><button>Manual control</button><ActivityTarget target="machine:a"><span>Equipment</span></ActivityTarget></SemanticActivityProvider>);
 expect(document.activeElement).toBe(button);expect(screen.getByRole('status').textContent).toContain('AI working');
 view.rerender(<SemanticActivityProvider value={[{id:'event-1',target:'machine:a',phase:'completed',label:'Capabilities read'}]}><button>Manual control</button><ActivityTarget target="machine:a"><span>Equipment</span></ActivityTarget></SemanticActivityProvider>);
 expect(document.activeElement).toBe(button);expect(screen.getByRole('status').textContent).toContain('AI completed');
});

it.each([false,true])('returns to Machines after confirmed removal, with another machine: %s',async(other)=>{
 h.connection=fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'}),...(other?[machineRow({id:'b',displayName:'Beta'})]:[])]});fleet();
 await openMachine('Alpha');
 fireEvent.click(await screen.findByRole('button',{name:/Machine details/}));
 fireEvent.click(screen.getByRole('button',{name:'Remove this machine'}));
 fireEvent.click(screen.getByRole('button',{name:'Remove Alpha'}));
 if(other){expect(await screen.findByRole('button',{name:/^Open Beta/})).toBeTruthy();expect(screen.queryByRole('navigation',{name:'Machine views'})).toBeNull();}
 else expect(await screen.findByRole('button',{name:'Connect your first machine'})).toBeTruthy();
 expect(screen.getByRole('heading',{name:'Machines'})).toBeTruthy();
 expect(screen.queryByLabelText('Name for Beta')).toBeNull();
 expect(screen.queryByText('That machine is no longer in this view')).toBeNull();
 expect(screen.queryByRole('button',{name:'Back to fleet'})).toBeNull();
});
it('keeps a refused removal on its machine details',async()=>{
 h.connection=fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'})]});
 h.connection.query.fleetRevokeMachine.mockRejectedValue(new Error('Removal refused'));fleet();
 await openMachine('Alpha');
 fireEvent.click(await screen.findByRole('button',{name:/Machine details/}));
 fireEvent.click(screen.getByRole('button',{name:'Remove this machine'}));
 fireEvent.click(screen.getByRole('button',{name:'Remove Alpha'}));
 expect(await screen.findByText('Removal refused')).toBeTruthy();
 expect(screen.getByLabelText('Name for Alpha')).toBeTruthy();
 expect(screen.getByRole('button',{name:'Remove Alpha'})).toBeTruthy();
});

it('selects the first model, keeps a chosen model across heartbeats and falls back when it disappears',async()=>{
 const {MachineEquipment}=await import('../../src/apps/fleet/FleetWorkspace');
 const {machineFromRow}=await import('../../src/apps/fleet/rows');
 const machine=machineFromRow(machineRow({id:'a',labels:{'model:alpha':'ctx=4096','model:beta':'ctx=8192'}}));
 const onInspect=vi.fn(); const now=new Date();
 const view=render(<MachineEquipment machine={machine} now={now} onInspect={onInspect}/>);
 expect(screen.getByRole('button',{name:'alpha Chat'}).getAttribute('aria-pressed')).toBe('true');
 expect(screen.getByRole('region',{name:'Selected equipment'}).textContent).toContain('alpha');
 fireEvent.click(screen.getByRole('button',{name:'beta Chat'}));
 view.rerender(<MachineEquipment machine={{...machine,activeCount:2}} now={now} onInspect={onInspect}/>);
 expect(screen.getByRole('region',{name:'Selected equipment'}).textContent).toContain('beta');
 view.rerender(<MachineEquipment machine={{...machine,reportedLabels:{'model:alpha':'ctx=4096'}}} now={now} onInspect={onInspect}/>);
 expect(screen.getByRole('region',{name:'Selected equipment'}).textContent).toContain('alpha');
 fireEvent.click(screen.getByRole('button',{name:'Manage models'}));expect(onInspect).toHaveBeenLastCalledWith('models');
 fireEvent.click(screen.getByRole('button',{name:'Call history'}));expect(onInspect).toHaveBeenLastCalledWith('activity');
 fireEvent.click(screen.getByRole('button',{name:'Personal inference'}));expect(onInspect).toHaveBeenLastCalledWith('sharing');
});
it('gives empty equipment and recent work their own empty states',async()=>{
 h.connection=fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'})]});fleet();
 await openMachine('Alpha');
 expect(await screen.findByRole('region',{name:'No local models'})).toBeTruthy();
 expect(screen.getByRole('region',{name:'No installed apps'})).toBeTruthy();
 expect(await screen.findByRole('region',{name:'No recent work'})).toBeTruthy();
 expect(screen.queryByRole('region',{name:'Selected equipment'})).toBeNull();
});
