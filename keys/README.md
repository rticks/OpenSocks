# OpenSocks opkg signing key

`d24a5e234001294c` is the OpenSocks repository public usign key. Install it as:

```sh
wget -O /etc/opkg/keys/d24a5e234001294c https://rel.n4t.su/opkg/d24a5e234001294c
```

The matching secret key is never stored in this repository. GitHub Actions
loads it only from the `OPKG_SIGNING_KEY` repository secret and refuses to
publish unsigned artifacts.

## apk signing key (OpenWrt 25.12+)

OpenWrt 25.12 verifies apk repositories with an EC P-256 key instead of a
usign key. Generate a pair the way OpenWrt does:

```sh
openssl ecparam -name prime256v1 -genkey -noout -out private.pem
openssl ec -in private.pem -pubout -out public.pem
```

The private key must never be committed. GitHub Actions reads it from the
`APK_SIGNING_KEY_PEM` repository secret (with the matching public key in
`APK_PUBLIC_KEY_PEM`) and publishes it as `opensocks.pem` next to the index. If
the secret is absent the apk feed is published unsigned, and clients must
install with `--allow-untrusted`.

Clients trust the key by dropping the public half into `/etc/apk/keys/` (for
APKv3 the file name is not significant; the key is matched by identity):

```sh
wget -O /etc/apk/keys/opensocks.pem https://rel.n4t.su/opkg/apk/opensocks.pem
```

Key rotation has no grace period: publishing with a new private key requires
publishing the matching `opensocks.pem` at the same time, because clients reject
an index whose signature does not match a key already present in `/etc/apk/keys/`.
