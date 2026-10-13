#!/usr/bin/env python3
"""Validate the exact missing release asset set, checksums and ELF targets."""
import hashlib
from pathlib import Path
import struct
import subprocess
import sys
import tarfile

GROUPS = {
    'freebsd': ['freebsd-amd64', 'freebsd-arm64', 'freebsd-i386'],
    'linux_musl': ['linux-musl-amd64', 'linux-musl-arm64', 'linux-musl-mips', 'linux-musl-mips64', 'linux-musl-mips64le', 'linux-musl-mipsle', 'linux-musl-ppc64le', 'linux-musl-s390x'],
    'linux_musl_arm': ['linux-musleabi-arm', 'linux-musleabihf-arm', 'linux-musleabi-armel', 'linux-musleabihf-armel', 'linux-musleabi-armv5l', 'linux-musleabihf-armv5l', 'linux-musleabi-armv6', 'linux-musleabihf-armv6', 'linux-musleabihf-armv7l', 'linux-musleabi-armv7m', 'linux-musleabihf-armv7r'],
}
ARCHES = {
    'amd64': (2, 1, 62), 'arm64': (2, 1, 183), 'i386': (1, 1, 3),
    'mips': (1, 2, 8), 'mipsle': (1, 1, 8), 'mips64': (2, 2, 8), 'mips64le': (2, 1, 8),
    'ppc64le': (2, 1, 21), 's390x': (2, 2, 22),
}
group = sys.argv[1]
folder = Path(sys.argv[2]) if len(sys.argv) > 2 else Path('build/compress')
expected = {f'alist-{target}.tar.gz' for target in GROUPS[group]}
actual = {p.name for p in folder.glob('*.tar.gz')}
assert actual == expected, (actual - expected, expected - actual)
md5_name = {'freebsd': 'md5-freebsd.txt', 'linux_musl': 'md5-linux-musl.txt', 'linux_musl_arm': 'md5-linux-musl-arm.txt'}[group]
checksums = {}
for line in (folder / md5_name).read_text().splitlines():
    digest, name = line.split(maxsplit=1)
    checksums[name.removeprefix('./')] = digest
assert set(checksums) == expected
for target in GROUPS[group]:
    name = f'alist-{target}.tar.gz'
    archive = folder / name
    assert hashlib.md5(archive.read_bytes()).hexdigest() == checksums[name]
    with tarfile.open(archive) as tar:
        members = tar.getmembers()
        assert len(members) == 1 and members[0].name == 'alist' and members[0].isfile(), name
        binary = tar.extractfile(members[0]).read()
    assert binary[:4] == b'\x7fELF', name
    arch = target.rsplit('-', 1)[1]
    elf_class, byte_order, machine = ARCHES.get(arch, (1, 1, 40))
    assert binary[4:6] == bytes([elf_class, byte_order]), name
    assert struct.unpack('<H' if byte_order == 1 else '>H', binary[18:20])[0] == machine, name
    if group == 'freebsd': assert binary[7] == 9, name
    # ldflags build metadata must refer to the original release, not the recovery commit.
    for text in [b'v3.65.0', b'ea628f9a']:
        assert text in binary, (name, text)
    print(name, 'ELF target and MD5 verified', hashlib.sha256(archive.read_bytes()).hexdigest())
    if target == 'linux-musl-amd64' and len(sys.argv) == 2:
        executable = Path('/tmp/alist-backfill-version')
        executable.write_bytes(binary)
        executable.chmod(0o755)
        version = subprocess.check_output([str(executable), 'version'], text=True)
        print(version)
        assert 'v3.65.0' in version and 'ea628f9a' in version
print(f'Validated {len(expected)} packages for {group}')
