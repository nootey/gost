import type { Activity, ActivityEvent, ActivityMetadata } from "../../domains/file/models.ts";

export interface EventView {
    icon: string;
    text: (metadata: ActivityMetadata) => string;
}

function folderName(folder: string | null | undefined): string {
    return folder ?? "Home";
}

// How each audit event reads. The text leaves out the item's name: the details dialog shows it in its header, and the
// activity page on the line above.
export const EVENT_VIEWS: Record<ActivityEvent, EventView> = {
    uploaded: { icon: "pi pi-upload", text: (m) => `Uploaded to ${folderName(m.folder)}` },
    created: { icon: "pi pi-plus", text: (m) => `Created in ${folderName(m.folder)}` },
    copied: { icon: "pi pi-copy", text: (m) => `Copied into ${folderName(m.folder)}` },
    renamed: { icon: "pi pi-pencil", text: (m) => `Renamed from ${m.old_name ?? ""} to ${m.name}` },
    moved: { icon: "pi pi-file-export", text: (m) => `Moved from ${folderName(m.from)} to ${folderName(m.folder)}` },
    trashed: { icon: "pi pi-trash", text: () => "Moved to the trash" },
    restored: { icon: "pi pi-replay", text: (m) => `Restored to ${folderName(m.folder)} as ${m.name}` },
    purged: { icon: "pi pi-times-circle", text: () => "Deleted forever" },
    expired: { icon: "pi pi-clock", text: () => "Deleted forever after 30 days in the trash" },
    hidden: { icon: "pi pi-eye-slash", text: () => "Hidden" },
    unhidden: { icon: "pi pi-eye", text: () => "Unhidden" },
};

export function eventText(entry: Activity): string {
    return EVENT_VIEWS[entry.event].text(entry.metadata);
}
