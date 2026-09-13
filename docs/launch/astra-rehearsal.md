# Astra rehearsal preset

In **Configuration**, pause the service and wait for all active work to finish.
Confirm the draft Codex executable path, then select **Load Codex models**.
The preset requires a successful catalog for that exact draft path with an
available, exact `gpt-6-astra` entry and compatible supported efforts. A typed
model ID is not catalog validation. Reload after changing the executable or a
catalog failure; missing, unavailable, or incompatible entries block application.

Choose **Astra rehearsal effort** from the returned list. `medium` is preselected
only when explicitly supported; otherwise select an effort yourself. Select
**Apply Astra rehearsal preset**, review the planned changes, then **Confirm
preset**. **Cancel** preserves the entire draft.

Confirmation replaces all four role routes, all execution tiers (XS through XL),
and Repair with Codex / `gpt-6-astra` / the selected effort, removing OpenCode
provider and variant fields from those routes. It sets nine discovery agents,
one concurrent task, one task per cycle, and a 21,600-second (six-hour) interval.
Every other setting, including unsaved verification commands, is preserved.

This changes only the unsaved draft. It does not save, check connections, start
an audit or execution, or set operating mode. The operator must **Save
configuration**, **Check connection**, then explicitly choose **Run once**.
Existing saved task contracts and shipped defaults are unchanged. The **Setup
checklist** at the top of Configuration reports the routes as entered until saved,
counts catalog matches for the entered executable, and links to this preset's
effort selector; it starts nothing and a catalog match is not a connection check.

Live rehearsal requires the owner's dedicated VM and bot with already approved
account setup, repository configuration, and verification commands. Check the
connection against the saved exact routes before running. Browser coverage uses
synthetic fixtures; it is not evidence of live Astra access, execution, or PR
delivery.
