# Review request retries

An automatic request is identified by store, order and product. Before handing
it to Campaigns, Reviews durably writes its hash and an authenticated encrypted
retry envelope. The envelope contains the same random bearer and original
expiry. It is encrypted with the existing installation `MEMQL_MASTER_KEY`,
bound to the request and its merchant, store, organization, site, order, product
and recipient. Reviews also supplies the buyer digest to Campaigns, which
checks it against its authoritative order read immediately before constructing
the recipient; changing the order email after the due scan cannot move the
original bearer to another buyer. Request rows and exports never contain the plaintext bearer.

A pending or failed request reuses that envelope. Campaigns can then replay an
accepted receipt or reconcile an uncertain handover without replacing the link
that reached the buyer. Retries never extend expiry. A failed final request
write is counted as failed and leaves the durable envelope available for the
next scan. Settled current versions discard it; historical ciphertext is
removed with personal data when the reviewer is erased. Store exports omit
both token hashes and encrypted retry envelopes.

Request issuance, retries and erasure use a shared PostgreSQL advisory gate on
the dedicated gate pool. Fresh reads and mutation row-version fences prevent a
worker whose gate was lost from overwriting a newer request. Manual issuance
participates in this protocol; erasure first writes a terminal request version,
then scrubs all historical versions. Each held phase is bounded to 55 seconds.
Nested Campaigns gates borrow that gate transaction rather than exhausting the
pool while the request gate is held.

An unresolved legacy request with only a hash cannot safely recreate its bearer.
It is left unchanged and is not automatically mailed. The same applies to an
expired link, a changed recipient or serving identity, a missing encryption
key, or a damaged envelope. An operator must investigate the Campaigns receipt
and arrange any deliberate manual replacement. Never clear a request or receipt
to force an automatic resend.

Keep invitations paused while upgrading replicas: old engines do not participate
in the request gate/fence protocol and can still rotate unresolved tokens. Resume
only after every sending replica has the new code and the same installation
master key. Key replacement without access to the original key makes unresolved
envelopes unrecoverable and fails closed; this change does not introduce a key
rotation mechanism.
