/** A thrown value as the sentence the surface renders -- the server's own
 *  words when it is an Error, never a paraphrase. */
export function errorSentence(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
