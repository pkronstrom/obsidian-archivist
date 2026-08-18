import { App, FuzzySuggestModal } from "obsidian";

/**
 * Pick a vault from the ones the token opens.
 *
 * A real picker, not a listing. The whole argument for path-qualified
 * addressing was that it makes a picker possible -- a listing that tells you
 * the answer and then leaves you to type it is the version of this feature
 * that does not pay for itself.
 */
export class VaultPickerModal extends FuzzySuggestModal<string> {
	constructor(
		app: App,
		private readonly vaults: string[],
		private readonly onPick: (vault: string) => void,
	) {
		super(app);
		this.setPlaceholder("Which vault should this device sync?");
	}

	getItems(): string[] {
		return this.vaults;
	}

	getItemText(vault: string): string {
		return vault;
	}

	onChooseItem(vault: string): void {
		this.onPick(vault);
	}
}
