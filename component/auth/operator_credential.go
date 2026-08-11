package auth

// EnvOperatorKey names the environment variable carrying the cluster OPERATOR
// credential: the bearer token `Authorization: Operator <key>` that admits a
// gRPC stream as a synthetic cluster owner
// (component/grpc/operator_stream_interceptor.go).
//
// It lives in the AUTH package, not beside secret.EnvMasterKey, because it is
// an authentication credential rather than an encryption key -- and because
// putting the two names in one file is a short step from putting the two
// VALUES in one variable, which is exactly the defect memql#3519 undoes.
//
// The operator path used to authenticate against secret.EnvMasterKey, argued
// for on the grounds that the master key was "already the operator credential
// of the cluster (anyone who can read /var/lib/memql secrets from the host can
// produce it)". That premise is about HOST FILESYSTEM ACCESS, and it stopped
// describing where the value actually lived:
//
//   - `genesis-seal --sync-shell` DEFAULTED TO TRUE and wrote the master key
//     into ~/.bashrc / ~/.zshrc at each file's existing (typically 0644) mode
//     -- world-readable to every local account, and carried into dotfile
//     backups, sync tools and screen shares.
//   - ESO delivers it from Key Vault into the `memql-secrets` k8s Secret,
//     which every mesh Deployment consumes via envFrom, so the operator path
//     is live on STAGING AND PRODUCTION.
//
// Composed: a value in a world-readable dotfile was a cluster-owner bearer
// token against production over the network. The split costs one more secret
// to rotate and restores the property the old comment assumed it had -- the
// thing that DECRYPTS is not the thing that AUTHENTICATES.
//
// Unset means the operator path is unavailable and every `Operator` stream is
// refused. That is the safe default and the documented way to disable it.
const EnvOperatorKey = "MEMQL_OPERATOR_KEY"
