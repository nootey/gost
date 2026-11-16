import vueHelper from "./vue_helper.ts";
import type { StoredFile } from "../../domains/file/models.ts";

export interface ItemView {
    iconBox: string;
    icon: string;
    type: string;
    size: string;
    row: string;
}

const FOLDER_VIEW: Omit<ItemView, "size"> = {
    iconBox: "bg-accent-soft text-accent",
    icon: "pi pi-folder",
    type: "Folder",
    row: "cursor-pointer",
};

// A folder's size_bytes is the total of the files under it, so a folder shows a size like a file.
export function itemView(item: StoredFile): ItemView {
    const size = vueHelper.formatBytes(item.size_bytes);
    if (item.is_folder) return { ...FOLDER_VIEW, size };
    return {
        iconBox: "bg-inset text-fg-muted",
        icon: vueHelper.fileIcon(item.content_type),
        type: vueHelper.mimeType(item.content_type),
        size,
        row: "",
    };
}
