importpath=github.com/daaku/archtools
pkgname=$(basename "$importpath")
pkgver=$(git rev-list --count HEAD)
pkgrel=1
pkgdesc='tools to manage archlinux systems'
arch=('x86_64' 'aarch64')
url="https://$importpath"
depends=('pacman')
makedepends=('go')

build() {
  cd ..
  go build -trimpath -o archdiff ./cmd/archdiff
  go build -trimpath -o archdest ./cmd/archdest
  go build -trimpath -o archpets ./cmd/archpets
}

package() {
  cd ..
  install -Dm755 archdiff "$pkgdir/usr/bin/archdiff"
  install -Dm755 archdest "$pkgdir/usr/bin/archdest"
  install -Dm755 archpets "$pkgdir/usr/bin/archpets"
}
