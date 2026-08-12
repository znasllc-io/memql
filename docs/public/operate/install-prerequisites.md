---
audience: public
status: current
area: operate
sinceVersion: 0.16.0
owner: platform
---

# Before you install a local cluster

The install wizard places everything memQL needs. There is a short list it
deliberately does not, and this page is that list plus the reasoning.

## What you do first

**Docker, installed and running.**

| Platform | How |
|---|---|
| macOS | Install Docker Desktop, then **open it** and wait for the whale icon to settle |
| Linux (Debian/Ubuntu) | `sudo apt-get install -y docker.io`, then `sudo systemctl enable --now docker` |
| Linux (Fedora/RHEL) | `sudo dnf install -y docker`, then `sudo systemctl enable --now docker` |

**On Linux, add yourself to the `docker` group and then LOG OUT AND BACK IN:**

```bash
sudo usermod -aG docker "$USER"
```

The second half is not optional and not something any installer can do for
you. A process's supplementary groups are fixed when it starts and inherited
from its parent, so nothing running inside your current login session can
acquire the group -- **including the editor**, which was started by the desktop
session and carries the same credentials. Restarting the editor does not help.
Log out and back in, or reboot.

The wizard detects all three states separately and says which one you are in:
`missing`, `stopped`, `denied` (you are not in the group) and `stale` (you are
in the group, but this session started before that was true).

## Why Docker is not installed for you

It is the one prerequisite where doing it automatically would be worse.

- On **Linux** a real Docker install means adding a third-party apt repository
  and a GPG key, installing a system daemon, and enabling a systemd unit. Those
  are changes to how the machine boots, made under a package source memQL chose
  for you.
- On **macOS** it is Docker Desktop -- a licensed GUI application that a person
  has to accept terms for and launch. There is no unattended form of that.
- And the group change ends in a re-login regardless, so automating the install
  would remove one manual step and leave the other.

Docker is also **never removed** by uninstalling memQL, for the same reason: it
is a platform other things on your machine may be using.

## What the wizard installs for you

Everything else, without asking beyond one password prompt:

| What | Where it goes | Removed on uninstall? |
|---|---|---|
| k3d, kubectl, mkcert | `~/.memql/bin/` | Only if you tick it -- general tools |
| The NSS tools (`certutil`) | your package manager | **No** -- see below |
| A local certificate authority | mkcert's `CAROOT` | Only if you tick it, and only if memQL created it |
| `cockpit.local.znas.io` etc. in `/etc/hosts` | a marked block | Yes, always -- memQL put it there |
| The memQL checkout | `~/.memql/src/` | Yes, always |
| The k3d cluster | Docker | Yes, always |

### The password prompt

Three of those steps need root: the hosts-file block, trusting the certificate
authority, and installing the NSS tools. The wizard asks through your desktop's
own password dialog. If there is no desktop to draw on -- over SSH, on a
headless box, in CI -- it stops and hands you the exact command to run in a
terminal instead.

### Why the NSS tools are never removed

On Linux, Firefox and Chrome do not read the system trust store; they read
their own NSS database, and `mkcert` can only write it through `certutil`.
Without it the certificate is issued, the cluster comes up, every check passes,
and the front door is untrusted in the one application the install's last step
tells you to open.

So memQL installs it -- and never takes it back. `certutil` is a general system
tool other software uses, and an application uninstaller that removes
distribution packages is how installers earn their reputation. The install
graph says so in the document itself: the step is marked `retained`, with the
reason attached.

## What an uninstall does

memQL's own artifacts go without being asked about: the cluster, the checkout,
the hosts block, the receipt. The general tools are offered **unticked** -- k3d,
kubectl, mkcert and the local CA -- with a note on each saying what else it is
good for. A certificate authority that predates the install is refused outright
even if you tick it, because the receipt records that memQL did not create it.

Refs: memql#3566 memql#3562 memql#3560
