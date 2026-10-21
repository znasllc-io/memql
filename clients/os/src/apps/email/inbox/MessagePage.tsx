import { Fact, Facts, Head, Panel, formatMoment } from "../../../kit";
import { Preview } from "../templates/Preview";
import type { CapturedEmail } from "./InboxSection";

export function MessagePage({ message, onBack }: { message: CapturedEmail; onBack: () => void }) {
  return <>
    <Head navigationKey={`message:${message.id}`} title={message.Subject} back={{ label: "Inbox", onSelect: onBack }} />
    <Panel label="Message details"><Facts>
      <Fact label="From" value={`${message.fromName} <${message.from}>`} />
      <Fact label="To" value={message.To} />
      <Fact label="Captured" value={formatMoment(message.createdAt)} />
      {message.Headers?.["Reply-To"] ? <Fact label="Reply to" value={message.Headers["Reply-To"]} /> : null}
    </Facts></Panel>
    <Preview key={message.id} html={message.HTMLBody} text={message.TextBody} />
  </>;
}
