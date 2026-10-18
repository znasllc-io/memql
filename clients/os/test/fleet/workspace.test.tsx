import { useState } from 'react';
import {act, cleanup, fireEvent, render, screen, within, waitFor} from '@testing-library/react';
import {afterEach, beforeEach, expect, it, vi} from 'vitest';
const h=vi.hoisted(()=>({connection:null as any}));
vi.mock('../../src/live/connection',()=>({useOsConnection:()=>h.connection,osBridgePath:'',bridgePathFor:()=>''}));
const {fakeConnection,machineRow,appSessionRow,shareDirectoryRow,withSession,rowsResult}=await import('./harness');
const {FleetApp}=await import('../../src/apps/fleet/FleetApp');
const {MachinesProvider,WORKER_REGISTRATION_CONCEPT}=await import('../../src/live/machines');
const {ActivityTarget,SemanticActivityProvider}=await import('../../src/kit/SemanticActivity');
const {installSeededAccess}=await import('../seededAccess');
const {readTheBar}=await import('../readTheBar');
afterEach(cleanup);
beforeEach(()=>{installSeededAccess('owner');h.connection=fakeConnection();});
// A machine is opened from the LIST: the section no longer selects the first one
// on sight, and there is no dropdown to pick another from.
const openMachine=async(name:string)=>fireEvent.click(await screen.findByRole('button',{name:new RegExp(`^Open ${name}`)}));
function fleet(){return render(withSession(<MachinesProvider><FleetApp sectionId="machines" navigate={vi.fn()} askContext={vi.fn()} store={{load:()=>({version:1,defaultSection:'machines',showRevoked:false}),save:()=>{}}}/></MachinesProvider>));}

function sharedMachine() {
 const row=machineRow({id:'a',displayName:'Alpha',sharing:{mode:'people',userIds:['ana'],groupIds:[]}});
 h.connection=fakeConnection({myWorkersWithStatus:[row],fleetShareDirectory:[shareDirectoryRow({id:'a',people:[{id:'ana',name:'Ana Ruiz',detail:''},{id:'bo',name:'Bo Chen',detail:''}]})]});
 return row;
}

it('updates the owner and shared call breakdown in Equipment from the existing machine feed', async () => {
 const row=sharedMachine(); fleet(); await openMachine('Alpha');
 const breakdown=screen.getByLabelText('Active call breakdown');
 expect(within(breakdown).getAllByText('—')).toHaveLength(2);
 const report={nodeId:'agent-test',observedAt:new Date().toISOString(),ownerCalls:2,sharedCalls:3,users:[{id:'ana',activeCalls:3}],groups:[{id:'design',activeCalls:3}]};
 await act(async()=>h.connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT,{...row,activeCount:99,sharingActivity:report},'NODE_UPDATED'));
 expect(within(breakdown).getByText('Your calls').nextElementSibling?.textContent).toBe('2');
 expect(within(breakdown).getByText('Shared calls').nextElementSibling?.textContent).toBe('3');
 expect(screen.getByText('5 active calls')).toBeTruthy();
 expect(screen.queryByText('99 active calls')).toBeNull();
 await act(async()=>h.connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT,{...row,connectedNodeId:'agent-next',sharingActivity:report},'NODE_UPDATED'));
 expect(within(breakdown).getAllByText('—')).toHaveLength(2);
 expect(h.connection.query.myWorkersWithStatus).toHaveBeenCalledTimes(1);
 fireEvent.click(within(screen.getByLabelText('Machine hardware')).getByRole('button',{name:'Activity'}));
 expect(within(screen.getByRole('navigation',{name:'Machine views'})).getByRole('button',{name:'Activity'}).getAttribute('aria-current')).toBeTruthy();
});

it('places Settings after Activity and groups configuration, sharing and reported facts on that page',async()=>{
 sharedMachine();fleet();await openMachine('Alpha');
 const tabs=screen.getByRole('navigation',{name:'Machine views'});
 expect(within(tabs).getAllByRole('button').map(button=>button.textContent)).toEqual(['Equipment','Models','Apps','Activity','Settings']);
 fireEvent.click(within(tabs).getByRole('button',{name:'Settings'}));
 expect(within(screen.getByRole('region',{name:'General'})).getByLabelText('Name for Alpha')).toBeTruthy();
 expect(within(screen.getByRole('region',{name:'Sharing'})).getByRole('switch',{name:'Share models'})).toBeTruthy();
 expect(screen.getByRole('region',{name:'System'})).toBeTruthy();
 expect(await screen.findByRole('button',{name:'Pause sharing with Ana Ruiz'})).toBeTruthy();
});

it('retains Settings drafts across machine tabs and updates sharing counts from the machine feed',async()=>{
 const row=sharedMachine();fleet();await openMachine('Alpha');
 fireEvent.click(screen.getByRole('button',{name:'Settings'}));
 const search=await screen.findByRole('combobox',{name:'Search users and groups'});
 await screen.findByRole('button',{name:'Pause sharing with Ana Ruiz'});
 fireEvent.change(screen.getByLabelText('Name for Alpha'),{target:{value:'Unsaved name'}});
 fireEvent.change(search,{target:{value:'Bo'}});
 fireEvent.click(screen.getByRole('button',{name:'Activity'}));
 fireEvent.click(screen.getByRole('button',{name:'Equipment'}));
 await act(async()=>h.connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT,{...row,sharingActivity:{nodeId:'agent-test',observedAt:new Date().toISOString(),users:[{id:'ana',activeCalls:3}],groups:[]}},'NODE_UPDATED'));
 fireEvent.click(screen.getByRole('button',{name:'Settings'}));
 expect((screen.getByLabelText('Name for Alpha') as HTMLInputElement).value).toBe('Unsaved name');
 expect((screen.getByRole('combobox',{name:'Search users and groups'}) as HTMLInputElement).value).toBe('Bo');
 fireEvent.change(screen.getByRole('combobox',{name:'Search users and groups'}),{target:{value:''}});
 expect(screen.getByText('3 active calls')).toBeTruthy();
 expect(h.connection.query.myWorkersWithStatus).toHaveBeenCalledTimes(1);
 // Directory facts invalidate when their machine source changes.
 await waitFor(() => expect(h.connection.query.fleetShareDirectory).toHaveBeenCalledTimes(2));
});

it('keeps sharing and general write failures beside their own controls and supports sharing retry',async()=>{
 sharedMachine();fleet();await openMachine('Alpha');
 fireEvent.click(screen.getByRole('button',{name:'Settings'}));
 const sharing=screen.getByRole('region',{name:'Sharing'}), general=screen.getByRole('region',{name:'General'});
 h.connection.query.fleetSetSharing.mockRejectedValueOnce(new Error('Sharing refused'));
 fireEvent.click(await screen.findByRole('button',{name:'Pause sharing with Ana Ruiz'}));
 expect(await within(sharing).findByText('Sharing refused')).toBeTruthy();
 expect(within(general).queryByText('The cluster refused that change.')).toBeNull();
 h.connection.query.renameWorker.mockRejectedValueOnce(new Error('Rename refused'));
 fireEvent.change(screen.getByLabelText('Name for Alpha'),{target:{value:'New name'}});
 fireEvent.click(screen.getByRole('button',{name:'Rename'}));
 expect(await within(general).findByText('Rename refused')).toBeTruthy();
 expect(within(sharing).queryByText('Rename refused')).toBeNull();
 fireEvent.click(within(sharing).getByRole('button',{name:'Try again'}));
 expect(await within(sharing).findByRole('button',{name:'Resume sharing with Ana Ruiz'})).toBeTruthy();
 expect(within(sharing).queryByText('Sharing could not be updated.')).toBeNull();
});
it('returns to the list when the selected machine disappears, never to another machine',async()=>{
 h.connection=fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'}),machineRow({id:'b',displayName:'Beta'})]});fleet();
 await openMachine('Beta');
 fireEvent.click(await screen.findByRole('button',{name:'Settings'}));expect(screen.getByLabelText('Name for Beta')).toBeTruthy();
 await act(async()=>h.connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT,machineRow({id:'b',displayName:'Beta',revokedAt:new Date().toISOString()}),'NODE_UPDATED'));
 expect(await screen.findByRole('button',{name:/^Open Alpha/})).toBeTruthy();expect(screen.queryByRole('navigation',{name:'Machine views'})).toBeNull();expect(screen.queryByLabelText('Name for Alpha')).toBeNull();expect(screen.queryByText('That machine is no longer in this view')).toBeNull();
});
// The route composer's exact-chain-and-revision save, and its refused draft,
// are asserted where the composer lives: routingComposer.test.tsx.
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
 fireEvent.click(await screen.findByRole('button',{name:'Settings'}));
 fireEvent.click(screen.getByRole('button',{name:'Remove this machine'}));
 await readTheBar();
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
 fireEvent.click(await screen.findByRole('button',{name:'Settings'}));
 fireEvent.click(screen.getByRole('button',{name:'Remove this machine'}));
 await readTheBar();
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
 expect(screen.getByRole('status').textContent).toBe('1–2 of 2 models');
 expect(within(screen.getByRole('list', {name:'Model selection'})).getAllByRole('button')).toHaveLength(2);
 expect(screen.getByRole('button',{name:'alpha Conversation'}).getAttribute('aria-pressed')).toBe('true');
 expect(screen.getByRole('region',{name:'Selected equipment'}).textContent).toContain('alpha');
 fireEvent.click(screen.getByRole('button',{name:'beta Conversation'}));
 view.rerender(<MachineEquipment machine={{...machine,activeCount:2}} now={now} onInspect={onInspect}/>);
 expect(screen.getByRole('region',{name:'Selected equipment'}).textContent).toContain('beta');
 view.rerender(<MachineEquipment machine={{...machine,reportedLabels:{'model:alpha':'ctx=4096'}}} now={now} onInspect={onInspect}/>);
 expect(screen.getByRole('region',{name:'Selected equipment'}).textContent).toContain('alpha');
 fireEvent.click(screen.getByRole('button',{name:'Manage models'}));expect(onInspect).toHaveBeenLastCalledWith('models');
 fireEvent.click(screen.getByRole('button',{name:'Activity'}));expect(onInspect).toHaveBeenLastCalledWith('activity');
 expect(screen.queryByRole('button',{name:'Machine details'})).toBeNull();
 expect(screen.queryByRole('button',{name:'Personal inference'})).toBeNull();
 expect(screen.queryByText('Tools')).toBeNull();
});
it('identifies image generation separately from chat in the equipment selector',async()=>{
 const {MachineEquipment}=await import('../../src/apps/fleet/FleetWorkspace');
 const {machineFromRow}=await import('../../src/apps/fleet/rows');
 const machine=machineFromRow(machineRow({id:'a',labels:{'model:flux2-klein-4b':'imagegen=1','model:qwen3.5:4b':'ctx=262144,tools=1,vision=1'}}));
 render(<MachineEquipment machine={machine} now={new Date()} onInspect={vi.fn()}/>);
 expect(screen.getByRole('button',{name:'flux2-klein-4b Image generation'})).toBeTruthy();
 expect(screen.getByRole('button',{name:'qwen3.5:4b Conversation, tools & vision'})).toBeTruthy();
});

it('gives empty equipment and recent work their own empty states',async()=>{
 h.connection=fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'})]});fleet();
 await openMachine('Alpha');
 expect(await screen.findByRole('region',{name:'No local models'})).toBeTruthy();
 expect(screen.getByRole('region',{name:'No installed apps'})).toBeTruthy();
 expect(await screen.findByRole('region',{name:'No recent work'})).toBeTruthy();
 expect(screen.queryByRole('region',{name:'Selected equipment'})).toBeNull();
});

const carouselLabels = Object.fromEntries(['alpha','beta','charlie','delta','echo','foxtrot','golf'].map(name => [`model:${name}`, 'ctx=8192']));

it('pages reported models without changing selection or the machine inventory', async () => {
 const {MachineEquipment} = await import('../../src/apps/fleet/FleetWorkspace');
 const {machineFromRow} = await import('../../src/apps/fleet/rows');
 const machine = machineFromRow(machineRow({id:'a', labels:carouselLabels}));
 const onInspect = vi.fn();
 const now = new Date();
 const view = render(<MachineEquipment machine={machine} now={now} onInspect={onInspect}/>);
 const previous = screen.getByRole('button', {name:'Previous models'}) as HTMLButtonElement;
 const next = screen.getByRole('button', {name:'Next models'}) as HTMLButtonElement;
 expect(previous.disabled).toBe(true);
 expect(screen.getByRole('status').textContent).toBe('1–2 of 7 models');
 fireEvent.click(next);
 expect(screen.getByRole('status').textContent).toBe('3–4 of 7 models');
 expect(screen.queryByRole('button', {name:'alpha Conversation'})).toBeNull();
 expect(screen.getByRole('region', {name:'Selected equipment'}).textContent).toContain('alpha');
 fireEvent.click(screen.getByRole('button', {name:'charlie Conversation'}));
 expect(screen.getByRole('region', {name:'Selected equipment'}).textContent).toContain('charlie');
 expect(JSON.parse(screen.getByRole('region', {name:'Selected equipment'}).getAttribute('data-os-page-context')!)).toEqual({selectedModel:'charlie'});
 view.rerender(<MachineEquipment machine={{...machine, activeCount:3}} now={now} onInspect={onInspect}/>);
 expect(screen.getByRole('status').textContent).toBe('3–4 of 7 models');
 expect(screen.getByRole('button', {name:'charlie Conversation'}).getAttribute('aria-pressed')).toBe('true');
 fireEvent.click(next);
 fireEvent.click(next);
 expect(screen.getByRole('status').textContent).toBe('7 of 7 models');
 expect(next.disabled).toBe(true);
 fireEvent.click(previous);
 expect(screen.getByRole('status').textContent).toBe('5–6 of 7 models');
 expect(onInspect).not.toHaveBeenCalled();
 expect(machine.reportedLabels).toEqual(carouselLabels);
});

it('keeps keyboard focus visible on resize and clamps pages when inventory disappears', async () => {
 const {MachineEquipment} = await import('../../src/apps/fleet/FleetWorkspace');
 const {machineFromRow} = await import('../../src/apps/fleet/rows');
 let resize!: (width: number) => void;
 const disconnect = vi.fn();
 vi.stubGlobal('ResizeObserver', class {
   constructor(callback: ResizeObserverCallback) {
     resize = width => callback([{contentRect:{width}} as ResizeObserverEntry], this as unknown as ResizeObserver);
   }
   observe() {}
   disconnect = disconnect;
 });
 try {
   const machine = machineFromRow(machineRow({id:'a', labels:carouselLabels}));
   const now = new Date();
   const onInspect = vi.fn();
   const view = render(<MachineEquipment machine={machine} now={now} onInspect={onInspect}/>);
   const beta = screen.getByRole('button', {name:'beta Conversation'});
   beta.focus();
   act(() => resize(300));
   expect(screen.getByRole('status').textContent).toBe('2 of 7 models');
   expect(document.activeElement).toBe(beta);
   expect(screen.getByRole('button', {name:'beta Conversation'})).toBe(beta);
   act(() => resize(0));
   expect(screen.getByRole('status').textContent).toBe('2 of 7 models');
   act(() => resize(500));
   expect(screen.getByRole('status').textContent).toBe('1–2 of 7 models');
   expect(document.activeElement).toBe(beta);
   beta.blur();
   const next = screen.getByRole('button', {name:'Next models'});
   for (let i=0; i<3; i++) fireEvent.click(next);
   expect(screen.getByRole('status').textContent).toBe('7 of 7 models');
   view.rerender(<MachineEquipment machine={{...machine, reportedLabels:{'model:alpha':'ctx=4096'}}} now={now} onInspect={onInspect}/>);
   expect(screen.getByRole('status').textContent).toBe('1 of 1 model');
   expect((screen.getByRole('button', {name:'Next models'}) as HTMLButtonElement).disabled).toBe(true);
   view.rerender(<MachineEquipment machine={{...machine, reportedLabels:{}}} now={now} onInspect={onInspect}/>);
   expect(screen.queryByRole('button', {name:'Next models'})).toBeNull();
   expect(screen.getByRole('region', {name:'No local models'})).toBeTruthy();
   view.rerender(<MachineEquipment machine={machine} now={now} onInspect={onInspect}/>);
   act(() => resize(300));
   expect(screen.getByRole('status').textContent).toBe('1 of 7 models');
   view.unmount();
   expect(disconnect).toHaveBeenCalled();
 } finally { vi.unstubAllGlobals(); }
});


it('counts only the authorized filtered machine rows after the feed settles', async () => {
 let finish!: (value: ReturnType<typeof rowsResult>) => void;
 h.connection = fakeConnection();
 h.connection.query.myWorkersWithStatus.mockImplementation(() => new Promise(resolve => { finish = resolve; }));
 fleet();
 const count = () => screen.getByRole('heading', { name: 'Machines' }).closest('.os-head')?.querySelector('.os-head-meta')?.textContent;
 expect(count()).toBeUndefined();
 await act(async () => finish(rowsResult([machineRow({id:'a',displayName:'Alpha'}), machineRow({id:'b',displayName:'Beta'})])));
 expect(count()).toBe('2');
 fireEvent.click(screen.getByRole('button', { name: 'Find machines' }));
 fireEvent.change(screen.getByPlaceholderText('Search machines'), { target: { value: 'Alpha' } });
 expect(count()).toBe('1');
 expect(screen.queryByRole('button', { name: /^Open Beta/ })).toBeNull();
 await act(async () => h.connection.subscriptions.emit(WORKER_REGISTRATION_CONCEPT, {id:'a'}, 'NODE_DELETED'));
 expect(count()).toBe('0');
});

it('never presents a refused machine inventory as a zero count', async () => {
 h.connection = fakeConnection();
 h.connection.query.myWorkersWithStatus.mockRejectedValue(new Error('Inventory denied'));
 fleet();
 await screen.findByRole('region', { name: 'Machines unavailable' });
 expect(screen.getByRole('heading', { name: 'Machines' }).closest('.os-head')?.querySelector('.os-head-meta')).toBeNull();
});

it('All app sessions returns to the full list after a previously opened session', async () => {
 const session = appSessionRow({id:'session',workerId:'a',runId:'A recorded task'});
 h.connection = fakeConnection({myWorkersWithStatus:[machineRow({id:'a',displayName:'Alpha'})],appSessionsForUser:[session,...Array.from({length:5},(_,i)=>appSessionRow({id:`older-${i}`,workerId:'a',runId:`Older task ${i}`,startedAt:'2026-01-01T00:00:00Z'}))],appSessionById:[session]});
 function App() { const [section,setSection]=useState('machines');return <><button onClick={()=>setSection('machines')}>Machines section</button><FleetApp sectionId={section} navigate={setSection} askContext={vi.fn()} store={{load:()=>({version:1,defaultSection:'machines',showRevoked:false}),save:()=>{}}}/></>; }
 render(withSession(<MachinesProvider><App/></MachinesProvider>));
 await openMachine('Alpha');
 fireEvent.click(await screen.findByRole('button',{name:/A recorded task/}));
 await screen.findByText('Recording');
 fireEvent.click(screen.getByRole('button',{name:'Machines section'}));
 fireEvent.click(screen.getByRole('button',{name:'All app sessions'}));
 expect(await screen.findByRole('button',{name:/A recorded task/})).toBeTruthy();
 expect(h.connection.query.appSessionsForUser).toHaveBeenCalledTimes(1);
});
