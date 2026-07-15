// Package rights enumerates the fine-grained rights this service declares to the
// holistic rights standard. Each constant is the Linux group backing one permission in
// permissions/scrapr.json — keep the two in sync. Enforcement uses auth.User.Can,
// i.e. the standard rule: isAdmin || group ∈ groups. A host without privleg has empty
// hp_* groups, so non-admins resolve to admin-only.
package rights

const (
	// GroupUse backs permissions/scrapr.json → scrapr:use. Members (and admins) may read
	// their own scrapers, runs and documents.
	GroupUse = "hp_scrapr_use"

	// GroupRun backs permissions/scrapr.json → scrapr:run. Members (and admins) may create
	// and edit scrapers and trigger crawls. A crawl reasons via aigentic's choose router,
	// which can escalate to the metered Claude API — so this right is the paid surface.
	GroupRun = "hp_scrapr_run"
)
