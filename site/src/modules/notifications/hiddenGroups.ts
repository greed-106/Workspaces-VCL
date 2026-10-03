/**
 * Notification groups that this deployment does not offer, keyed by the group
 * name the API reports. The AI and chat notification groups are hidden
 * everywhere in the UI.
 */
export const hiddenNotificationGroups: ReadonlySet<string> = new Set([
	"Chat Events",
	"AI Cost Control Events",
	"AI Cost Control Admin Events",
]);

export const isHiddenNotificationGroup = (group: string): boolean =>
	hiddenNotificationGroups.has(group);
