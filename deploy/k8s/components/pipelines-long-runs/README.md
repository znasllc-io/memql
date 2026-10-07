# Long pipeline runs

An operator may compose this component in an instance overlay to allow pipeline
runs up to eight hours. It sets the same value on every agent and Workbench
replica. The agent stamps the run deadline; Workbench enforces that deadline
across forwarding and recovery. This affects every pipeline in that installation.

No shipping instance overlay includes this profile by default. The ordinary
run ceiling remains two hours and the ordinary step timeout remains 20 minutes.
Repository declarations must still name a longer step timeout, up to six hours.
Queue time and earlier stages consume the run budget independently.

This changes time limits only. It provisions no capacity and changes no CPU,
memory, disk, concurrency, cleanup or artifact limits. For the engine's history
scan, a node must fit the explicit four-CPU/6-GiB step plus its collector, init
containers and the existing mesh. The default cloud pool does not establish
that capacity. Local container measurements do not qualify a cloud installation.

See [pipeline time limits](../../../../docs/public/operate/pipelines-substrate.md#time).
