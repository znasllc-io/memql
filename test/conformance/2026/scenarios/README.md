# Scenarios

Whole automations as the product ships them: the decide-and-apply sweeps, the
forge state machine, the deployment pipeline, the campaigns engine. A scenario
directory is a case directory like any other (see `../README.md`): its
`fixture.memql` holds the concepts, queries, mutations and logic the automation
reaches, and its case file is the automation.

The body-language epic (dsl-v1-bodies) writes the first scenarios, because
they exercise the statement forms it defines. Until then this directory holds
no cases.
