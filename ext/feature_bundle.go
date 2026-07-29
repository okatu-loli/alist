package ext

// FeatureBundle is a COMPOSITE feature (design §2.5): share and labels are not a
// single extension point but a bundle of several — an APIRoute to manage them, a
// public landing page (an APIRoute with Public=true), an EventHook to react to fs
// changes, a ScheduledJob for expiry cleanup — plus host Store/Sign to persist
// and to sign ID-anchored links. Rather than invent a new mega-interface, a
// FeatureBundle is a value that ALSO implements the existing extension-point
// interfaces it contributes; the host EXPANDS it onto the F1 wiring points
// (RegisterAPIRouteInstance / RegisterEventHookInstance / …) by asserting which
// interfaces it satisfies, reusing that plumbing rather than duplicating it.
//
// So a share bundle is one Go value implementing Extension + FeatureBundle +
// APIRoute (+ EventHook + ScheduledJob as needed); Bundle() only declares the
// host RESOURCES it needs (Store/Sign), which the host provisions and gates
// through the store / sign grants.
type FeatureBundle interface {
	Extension

	// Bundle declares the host resources this composite needs. The composed
	// extension-point implementations are discovered by interface assertion on the
	// bundle value itself (the host checks whether it also implements APIRoute /
	// EventHook / ScheduledJob), so this spec carries only the resource requests.
	Bundle() BundleSpec
}

// BundleSpec declares the host resources a FeatureBundle needs provisioned.
type BundleSpec struct {
	// NeedsStore requests the plugin's namespaced Store (design §2.5: share/labels
	// persist their tables). The actual access is still gated by the "store"
	// grant at the host boundary.
	NeedsStore bool `json:"needs_store,omitempty"`
	// NeedsSign requests the Signer (design §2.5: share signs ID-anchored,
	// expiring download links; the plugin never sees the key). Gated by the "sign"
	// grant.
	NeedsSign bool `json:"needs_sign,omitempty"`
}
