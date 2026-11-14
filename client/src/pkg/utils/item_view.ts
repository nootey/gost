import vueHelper from "./vue_helper.ts";
import type { StoredFile } from "../../domains/file/models.ts";

export interface ItemView {
    iconBox: string;
    icon: string;
    type: string;
    size: string;
    row: string;
}

const FOLDER_VIEW: ItemView = {
    iconBox: "bg-accent-soft text-accent",
    icon: "pi pi-folder",
    type: "Folder",
    size: "—",
    row: "cursor-pointer",
};

export function itemView(item: StoredFile): ItemView {
    if (item.is_folder) return FOLDER_VIEW;
    return {
        iconBox: "bg-inset text-fg-muted",
        icon: vueHelper.fileIcon(item.content_type),
        type: vueHelper.mimeType(item.content_type),
        size: vueHelper.formatBytes(item.size_bytes),
        row: "",
    };
}
