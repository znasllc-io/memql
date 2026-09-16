import { render } from '@testing-library/react';
import { expect, it } from 'vitest';
import { visiblePageContext } from '../../src/kit/pageContext';

it('uses only the current window’s visible declared context, excluding parked pages and form secrets',()=>{
 const view=render(<><section aria-label="Fleet"><div data-os-page-context={JSON.stringify({machine:'Studio',view:'History'})}><input type="password" defaultValue="secret-token"/><p>Undeclared text</p></div><div hidden><div data-os-page-context={JSON.stringify({machine:'Hidden'})}/></div><div inert><div data-os-page-context={JSON.stringify({view:'Parked'})}/></div></section><section aria-label="Deployables" data-os-page-context={JSON.stringify({site:'other-app'})}/></>);
 const context=visiblePageContext(view.getByRole('region',{name:'Fleet'}),'app:fleet section:machines');
 expect(context).toContain('Studio');expect(context).toContain('History');
 for(const hidden of ['Hidden','Parked','other-app','secret-token','Undeclared text']) expect(context).not.toContain(hidden);
});
it('reads fresh selection and filters each time Ask opens',()=>{
 const view=render(<div data-testid="window"><div data-os-page-context={JSON.stringify({site:'first',view:'Overview'})}/></div>);
 expect(visiblePageContext(view.getByTestId('window'),'app:deployables')).toContain('first');
 view.rerender(<div data-testid="window"><div data-os-page-context={JSON.stringify({site:'second',view:'Traffic'})}/><div data-os-page-context={JSON.stringify({search:'portal'})}/></div>);
 const context=visiblePageContext(view.getByTestId('window'),'app:deployables');
 expect(context).not.toContain('first');expect(context).toContain('second');expect(context).toContain('Traffic');expect(context).toContain('portal');
});

it('labels Ask with the selected item and page without exposing identifiers',async()=>{
 const {visiblePageLabel}=await import('../../src/kit/pageContext');
 const view=render(<div data-testid="window"><div data-os-page-context={JSON.stringify({machine:'Studio',machineId:'opaque-registration-id',view:'History'})}/></div>);
 expect(visiblePageLabel(view.getByTestId('window'),'Fleet')).toBe('Studio / History');
});
