package extension

import (
	"github.com/AlistGo/alist-ext"
	"github.com/alist-org/alist/v3/internal/driver"
)

// DeriveStorageCapabilities inspects a legacy driver.Driver and returns the set
// of ext capabilities it actually implements (9.0 §4). This is the "implemented"
// side against which a declared Manifest.Capabilities is validated.
//
// 9.1 note: built-in drivers still speak the legacy driver.Driver interface, so
// the probe checks the legacy optional interfaces (both the plain and the
// *Result variants). Ext-native drivers (9.2+) will be probed against the ext
// optional interfaces instead. Seam.
func DeriveStorageCapabilities(d driver.Driver) []ext.Capability {
	var caps []ext.Capability
	add := func(ok bool, c ext.Capability) {
		if ok {
			caps = append(caps, c)
		}
	}

	_, mkdir := d.(driver.Mkdir)
	_, mkdirR := d.(driver.MkdirResult)
	add(mkdir || mkdirR, ext.CapMkdir)

	_, move := d.(driver.Move)
	_, moveR := d.(driver.MoveResult)
	add(move || moveR, ext.CapMove)

	_, rename := d.(driver.Rename)
	_, renameR := d.(driver.RenameResult)
	add(rename || renameR, ext.CapRename)

	_, cp := d.(driver.Copy)
	_, cpR := d.(driver.CopyResult)
	add(cp || cpR, ext.CapCopy)

	_, rm := d.(driver.Remove)
	add(rm, ext.CapRemove)

	_, put := d.(driver.Put)
	_, putR := d.(driver.PutResult)
	add(put || putR, ext.CapPut)

	_, putRapid := d.(driver.PutRapid)
	add(putRapid, ext.CapPutRapid)

	_, putURL := d.(driver.PutURL)
	_, putURLR := d.(driver.PutURLResult)
	add(putURL || putURLR, ext.CapPutURL)

	_, getter := d.(driver.Getter)
	add(getter, ext.CapGetter)

	_, getRooter := d.(driver.GetRooter)
	add(getRooter, ext.CapGetRooter)

	_, other := d.(driver.Other)
	add(other, ext.CapOther)

	_, arcReader := d.(driver.ArchiveReader)
	add(arcReader, ext.CapArchiveReader)

	_, arcGetter := d.(driver.ArchiveGetter)
	add(arcGetter, ext.CapArchiveGetter)

	_, arcDecomp := d.(driver.ArchiveDecompress)
	_, arcDecompR := d.(driver.ArchiveDecompressResult)
	add(arcDecomp || arcDecompR, ext.CapArchiveDecompress)

	return caps
}

// validateCapabilities returns an error if any declared capability is not in the
// implemented set (declared ⊄ implemented). This is the loud registration-time
// failure required by 9.0 §4.
func validateCapabilities(declared, implemented []ext.Capability) error {
	have := make(map[ext.Capability]struct{}, len(implemented))
	for _, c := range implemented {
		have[c] = struct{}{}
	}
	var missing []ext.Capability
	for _, c := range declared {
		if _, ok := have[c]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return &CapabilityMismatchError{Missing: missing}
	}
	return nil
}
