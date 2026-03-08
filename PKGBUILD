importpath=github.com/daaku/archtools
pkgname=$(basename $importpath)
pkgver=$(git rev-list --count HEAD)
pkgrel=1
pkgdesc='tools to manage archlinux systems'
arch=(x86_64 armv6h armv7h)
url=https://$importpath

package() {
  cd ..
  go build -o $pkgdir/usr/bin/archdiff ./cmd/archdiff
  go build -o $pkgdir/usr/bin/archdest ./cmd/archdest
  go build -o $pkgdir/usr/bin/archpets ./cmd/archpets
}
