import { defineStore } from "pinia";

// A paste of a cut moves the items, and a paste of a copy copies them. Either one clears the clipboard.
export type ClipboardMode = "cut" | "copy";

// sourceId is the folder the items came from. Null is Home.
export interface ClipboardContent {
    mode: ClipboardMode;
    sourceId: number | null;
    ids: number[];
}

// sessionStorage keeps the clipboard when the user opens another folder or reloads, in this tab only. Logout clears it.
const STORAGE_KEY = "gost_clipboard";

function isId(value: unknown): value is number {
    return Number.isInteger(value) && (value as number) > 0;
}

function readStoredClipboard(): ClipboardContent | null {
    try {
        const value: unknown = JSON.parse(sessionStorage.getItem(STORAGE_KEY) ?? "null");
        if (typeof value !== "object" || value === null) return null;
        const { mode, sourceId, ids } = value as Record<string, unknown>;
        if ((mode !== "cut" && mode !== "copy") || !(sourceId === null || isId(sourceId))) return null;
        if (!Array.isArray(ids) || ids.length === 0 || !ids.every(isId)) return null;
        return { mode, sourceId, ids };
    } catch {
        return null;
    }
}

export const useClipboardStore = defineStore("clipboard", {
    state: () => ({
        content: readStoredClipboard(),
    }),
    getters: {
        count(): number {
            return this.content?.ids.length ?? 0;
        },
        isCut(): (id: number) => boolean {
            return (id) => this.content?.mode === "cut" && this.content.ids.includes(id);
        },
        // A paste into the folder the items came from does nothing.
        canPasteInto(): (folderId: number | null) => boolean {
            return (folderId) => this.content !== null && this.content.sourceId !== folderId;
        },
    },
    actions: {
        put(mode: ClipboardMode, sourceId: number | null, ids: number[]): void {
            this.save({ mode, sourceId, ids });
        },

        // drop takes items off the clipboard, such as items moved to the trash.
        drop(ids: number[]): void {
            if (!this.content) return;
            const left = this.content.ids.filter((id) => !ids.includes(id));
            this.save(left.length > 0 ? { ...this.content, ids: left } : null);
        },

        clear(): void {
            this.save(null);
        },

        save(content: ClipboardContent | null): void {
            this.content = content;
            if (content) sessionStorage.setItem(STORAGE_KEY, JSON.stringify(content));
            else sessionStorage.removeItem(STORAGE_KEY);
        },
    },
});
