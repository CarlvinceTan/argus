package agent

// worstCases are values real people enter. Random strings prove nothing, because
// a reviewer rightly answers "that never happens"; these are the names, addresses
// and pastes that actually arrive and break a layout.
var worstCases = []struct{ name, text string }{
	{"a long double-barrelled name", "Aleksandra Wiśniewska-Kowalczyk-Brzęczyszczykiewicz"},
	{"an email with no break points", "bartholomew.fitzgerald@northwind-industries-holdings.example.com"},
	{"a pasted URL", "https://docs.example.com/workspaces/9f8e7d6c5b4a/projects/launch-plan/revisions/2026-10-31T23:59:59Z?share=readonly&utm_source=newsletter"},
	{"right-to-left text", "مرحبا، هل يمكنك مراجعة الخطة قبل الاجتماع غدا؟"},
	{"wide characters", "明日の会議の前に、リリース計画をもう一度確認してください。よろしくお願いします。"},
	{"joined emoji", "👩🏽‍💻🧑🏻‍🚀👨‍👩‍👧‍👦🏳️‍🌈 sounds good!"},
	{"a pasted block of lines", "Notes from today:\n- ship the release\n- review the rollout plan\n\n\n- call back\n\n"},
	{"one character", "k"},
	{"a very long paragraph", "I went through the whole rollout plan again this morning and I think the order of the second and third phases should change, because the migration has to finish before anyone sees the new settings screen, otherwise the first people through will end up with a half-configured workspace and a support ticket that nobody can answer until the next deploy goes out."},
}
