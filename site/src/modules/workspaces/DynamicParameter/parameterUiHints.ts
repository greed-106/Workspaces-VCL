/**
 * UI hints for template parameters.
 *
 * Template parameters do not carry UI metadata through to the workspace forms,
 * so the hints that shape those forms live here, keyed by parameter name.
 *
 * Keep these in sync with the Kubernetes template in
 * deploy/coder-template-kubernetes/main.tf. Hints only shape the form: the
 * API still accepts every value, so workspaces can be created through the CLI
 * or API outside these ranges.
 */

export type NumericStepper = {
	min?: number;
	max?: number;
	step: number;
};

/** Numeric parameters rendered as a stepper with - and + buttons. */
export const numericSteppers: Record<string, NumericStepper> = {
	home_disk_size: { min: 100, max: 500, step: 100 },
	// The form offers 1 to 4 cards. The API stays unrestricted, so the CLI can
	// still request other counts, and the template refuses requests that exceed
	// what is actually free.
	gpu_count: { min: 1, max: 4, step: 1 },
};

type VisibilityRule = {
	/** Parameter whose value decides whether this parameter applies. */
	dependsOn: string;
	/** Values of `dependsOn` for which this parameter is hidden. */
	hiddenForValues: string[];
};

const visibilityRules: Record<string, VisibilityRule> = {
	gpu_count: { dependsOn: "gpu_model", hiddenForValues: ["none"] },
};

/**
 * Reports whether a parameter should be hidden, given the values currently
 * entered in the form.
 */
export const isParameterHidden = (
	name: string,
	currentValue: (parameterName: string) => string | undefined,
): boolean => {
	const rule = visibilityRules[name];
	if (!rule) {
		return false;
	}
	return rule.hiddenForValues.includes(currentValue(rule.dependsOn) ?? "");
};

/**
 * Editor entry points that are intentionally not offered. Users develop in the
 * browser through the code-server app, so the desktop client buttons and chips
 * are hidden even though Coder advertises them for every agent.
 */
const hiddenDisplayApps = ["vscode", "vscode_insiders"];

export const isDisplayAppHidden = (app: string): boolean =>
	hiddenDisplayApps.includes(app);
