# pkg/image

The layer store and its tags. The store is the build cache: `pkg/build` asks
it for a layer by the key of its inputs before building one.

## The image store is the build cache

A layer is an immutable, committed disk state owned by a driver:

- hcs: a differencing VHDX
- vz: an APFS-cloned bundle
- boxd: a boxd snapshot, named in `layer.json` and deleted with the layer
- docker: a Docker image committed from the stopped guest, named in
  `layer.json` and deleted with the layer
- fake: a directory tree

A layer's **ID is its cache key**. The key is a hash of the parent ID, driver,
guest OS, and the fully resolved steps, including a content digest of every
copied file. An existing layer is never rebuilt, and `--no-cache` salts the
keys instead of overwriting. The one exception is a snapshot
(`Engine.Snapshot`, `disco-vm snapshot INSTANCE [NAME[:TAG]]`): a stopped
instance's disk committed as a layer on its image's layer, under a random ID,
since nothing reproducible made it. A snapshot consumes the instance, as a
build step does, so a layer is always cut the same way, from a stopped guest
that is then destroyed; to go on from one, create an instance of it (ADR
26-10-15-882). Tags name
layers. A layer with no tag
and no child is an untagged image: `images` lists it as `<none>` (`images -a`
lists every untagged layer, a chain's intermediate layers too) and `rmi`
takes its ID. `rmi` untags, then
deletes layers that nothing else tags, parents, or runs, as `docker rmi` does.
A layer is written under a temporary name, then renamed into place and
recorded in one transaction, so a crash never leaves a half-layer under a
committed ID. Installation media a driver
downloads itself (`media: latest`) is not a layer; it is kept in
`cache/<driver>/` (`InstallSpec.CacheDir`), so it is fetched once per host. A
driver may hold a layer outside its directory (a boxd snapshot), so the store
has the driver release a layer whenever it drops one: on `rmi`, on an aborted
build, and when another build published the same layer first. What the
driver holds can also be deleted behind the store's back (`boxd snap rm`,
`docker rmi`), so a build asks the driver whether each cached layer is still
there (`LayerExists`). One that is gone is built again under a new ID, salted
as `--no-cache` salts one, and the store records that the new layer now
answers for the old one's inputs. A new ID is what makes every layer after it
build again: theirs were made from the disk that is gone, and steps are not
reproducible. Nothing is removed on the driver's word. The lost layer and the
images built on it stay until `rmi`, untagged once the build moves their tags.
For the same
reason a tag is told to the driver (`TagLayer`, `UntagLayer`): on boxd a tag
is a snapshot of its own, named for the layer and the tag, so the account's
snapshot listing says which snapshots are which images. Tags are still read
only from the store.
