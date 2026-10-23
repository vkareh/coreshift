# Embedded gpgme runtime libraries

openshift-controller-manager links against three shared libraries beyond
glibc: libgpgme.so.11, libgpg-error.so.0, and libassuan.so.0. They are
checked in here and embedded in the coreshift binary, so a run does not
depend on extracting them from a release payload first. At startup they
are written to a directory under the image cache, and the controller
manager runs with its library search path pointing there.

## Provenance

Copied from the openshift-controller-manager component image of this
release:

    release: quay.io/openshift-release-dev/ocp-release:4.22.19-x86_64
             (manifest digest sha256:c02df63adabc70ed309853f200a1e3cfd4ef38edf00fc2a3f832d5f99e8165eb)
    image:   quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:71d63b896fda00ce0d2f44e026a4284b09a0a44e97301517ab73b32049ca51b5

In that image the library sonames are symlinks; the copied files are the
versioned files they point at (libgpgme.so.11.24.1, libgpg-error.so.0.32.0,
libassuan.so.0.8.5), kept under their soname names.

    0ba972ec6ce71ecb0d07f1a5fa318c2f498eb671327ba444393cb3a1b6b497a4  libgpgme.so.11
    7c35a3105985309405449a6f609cab1b0971fd65d44d70c4780106c12673ba08  libgpg-error.so.0
    2802ab456d7ce9730af980620dc280917270b33efb514af96ed9fafd4f7237d9  libassuan.so.0

## Licenses

All three libraries are LGPL-family, distributed under LGPL-2.1-or-later.
The license files shipped with them in the image are copied verbatim into
licenses/:

    libgpgme.so.11     licenses/gpgme/         (COPYING.LESSER, COPYING, LICENSES)
    libgpg-error.so.0  licenses/libgpg-error/  (COPYING.LIB, COPYING)
    libassuan.so.0     licenses/libassuan/     (COPYING.LIB, COPYING)

Redistributing the libraries together with these texts follows the norm
for LGPL-licensed binaries.
