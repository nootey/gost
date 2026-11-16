import vueHelper from "./vue_helper.ts";
import type { StoredFile } from "../../domains/file/models.ts";

export interface ItemView {
    iconBox: string;
    icon: string;
    type: string;
    size: string;
}

const FOLDER_VIEW: Omit<ItemView, "size"> = {
    iconBox: "bg-accent-soft text-accent",
    icon: "pi pi-folder",
    type: "Folder",
};

// How the file viewer shows a file. "other" shows the name, the size and a download button.
export type ViewKind = "picture" | "video" | "pdf" | "text" | "other";

// The view for each sniffed type. The server never sniffs an SVG as an image, so a script in one never runs here. The
// text view shows HTML and XML as plain text, so they never run.
const VIEW_BY_TYPE: Record<string, ViewKind> = {
    "image/png": "picture",
    "image/jpeg": "picture",
    "image/gif": "picture",
    "image/webp": "picture",
    "image/bmp": "picture",
    "video/mp4": "video",
    "video/webm": "video",
    "application/pdf": "pdf",
    "text/plain": "text",
    "text/html": "text",
    "text/xml": "text",
};

// viewKind says how the file viewer shows the item, or null if it does not open there. A copying file has no bytes yet.
export function viewKind(item: StoredFile): ViewKind | null {
    if (item.is_folder || item.is_copying) return null;
    return VIEW_BY_TYPE[vueHelper.mimeType(item.content_type)] ?? "other";
}

// isPicture says if the item is a picture, which has a thumbnail and zooms in the viewer.
export function isPicture(item: StoredFile): boolean {
    return viewKind(item) === "picture";
}

// isViewable says if the item opens in the file viewer.
export function isViewable(item: StoredFile): boolean {
    return viewKind(item) !== null;
}

// shownName is the name a list shows. With the extensions setting off, a file loses its last extension: report.pdf
// shows as report. A folder, a dot file such as .env, and a name that ends in a dot keep the full name.
export function shownName(item: StoredFile, showExtensions: boolean): string {
    if (showExtensions || item.is_folder) return item.name;
    const dot = item.name.lastIndexOf(".");
    if (dot <= 0 || dot === item.name.length - 1) return item.name;
    return item.name.slice(0, dot);
}

// A folder's size_bytes is the total of the files under it, so a folder shows a size like a file.
export function itemView(item: StoredFile): ItemView {
    const size = vueHelper.formatBytes(item.size_bytes);
    if (item.is_folder) return { ...FOLDER_VIEW, size };
    return {
        iconBox: "bg-inset text-fg-muted",
        icon: vueHelper.fileIcon(item.content_type),
        type: vueHelper.mimeType(item.content_type),
        size,
    };
}
