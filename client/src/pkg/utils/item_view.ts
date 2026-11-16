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

// The sniffed types an <img> shows. The server never sniffs an SVG as an image, so a script in one never runs here.
const PICTURE_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp"]);

// isPicture says if the picture viewer can show the item. A copying file has no bytes yet, so it does not show.
export function isPicture(item: StoredFile): boolean {
    return !item.is_folder && !item.is_copying && PICTURE_TYPES.has(vueHelper.mimeType(item.content_type));
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
