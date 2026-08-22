// Reading a deploy-console refusal that is a PRECONDITION rather than a fault.
//
// component/deploycontrol returns FailedPrecondition with a machine-readable
// reason leading the message (memql#4265), because "there is no overlay" is
// two different situations wanting two different sentences:
//
//   local_cluster        this installation is not a deploy target at all.
//                        Local clusters are operated with `make up`.
//   no_overlay_checkout  this node has no deploy checkout on disk, so what is
//                        pinned cannot be read.
//
// Neither is an error the operator can fix by retrying, and neither should
// render as a file path. What the portal used to show was the raw ENOENT
// followed by "Nothing is pinned in the overlay" -- which reads as "nothing is
// deployed", is false, and left the Ship controls clickable underneath it.

export type OverlayAbsence = "local_cluster" | "no_overlay_checkout" | "";

export function overlayAbsenceOf(error: string): OverlayAbsence {
  if (error.includes("local_cluster:")) return "local_cluster";
  if (error.includes("no_overlay_checkout:")) return "no_overlay_checkout";
  return "";
}

// The sentence for each, in the interface's own voice. Deliberately NOT the
// server's message verbatim: the server explains itself to any client, and
// this surface can say what it means for the person in front of it.
export function overlayAbsenceStatement(reason: OverlayAbsence): string {
  switch (reason) {
    case "local_cluster":
      return (
        "This is a local cluster, so there is nothing here to deploy. Local " +
        "installations are brought up and changed with `make up` (k3d + ArgoCD) " +
        "rather than through this console. The deployment history below is still real."
      );
    case "no_overlay_checkout":
      return (
        "This node has no deploy checkout on disk, so what is currently pinned " +
        "cannot be read. The console reads the cloud overlay under " +
        "MEMQL_DEPLOY_REPO_ROOT, which is not set here. Deploy actions are hidden " +
        "rather than offered, because they would read the same missing file."
      );
    default:
      return "";
  }
}
