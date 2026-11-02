# pkg/machine/docker review

- **A mount names a volume the daemon makes if it is missing.** A container
  created with a `type=volume` mount of a volume that is gone gets a new one,
  empty and unlabeled, under that name, and no error. Anything that mounts a
  volume something else may remove (a cache generation, which a save prunes)
  checks its labels once the container holds it, and a volume found by label
  is never found again once it is unlabeled. A review found a Create racing
  a save into an empty cache this way.
- **A container inherits its image's labels, and `docker commit` copies the
  container's into the image.** Every label an instance's container carries
  about itself is blanked by the commit and set, empty if need be, at
  Create; one that is not leaks into every instance of a snapshot. Verify
  found instances of a snapshot reporting its instance's data and cache
  volumes this way.
- **Find what an instance owns by its labels, not through its container,
  and remember a label names a name, not one instance.** A Destroy that
  removes the container first and fails after has nothing left to inspect,
  so its retry would leak the rest; the data and cache volumes are listed by
  `ai.discobox.vm.project` and `.instance`. Every instance of a name shares
  those labels, so a removal by them leaves a volume a container mounts (the
  daemon refuses it), and a Create checks, once its container mounts them,
  that its volumes are still the labeled ones it made, since a Destroy of an
  earlier instance of the name may have removed them in between.
