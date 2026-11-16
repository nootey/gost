const SIZE_UNITS = ["KB", "MB", "GB", "TB"];

const ICON_BY_TYPE: Partial<Record<string, string>> = {
    "application/pdf": "pi pi-file-pdf",
    "application/zip": "pi pi-box",
    "application/x-gzip": "pi pi-box",
    "application/x-rar-compressed": "pi pi-box",
};

const ICON_BY_GROUP: Partial<Record<string, string>> = {
    image: "pi pi-image",
    video: "pi pi-video",
    audio: "pi pi-volume-up",
    text: "pi pi-file-edit",
};

// Keyed by the 24-hour clock setting.
const DATE_FORMATS: Record<`${boolean}`, Intl.DateTimeFormat> = {
    true: new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short", hourCycle: "h23" }),
    false: new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short", hourCycle: "h12" }),
};

const RELATIVE_FORMAT = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

const vueHelper = {
    formatBytes(bytes: number): string {
        if (bytes < 1024) return `${bytes} B`;
        let value = bytes / 1024;
        let unit = 0;
        while (value >= 1024 && unit < SIZE_UNITS.length - 1) {
            value /= 1024;
            unit++;
        }
        return `${value.toFixed(value < 10 ? 1 : 0)} ${SIZE_UNITS[unit]}`;
    },

    mimeType(contentType: string): string {
        return contentType.split(";")[0].trim();
    },

    // charset gives the text encoding of a content type, such as utf-16le. The sniffer always names one for text.
    charset(contentType: string): string {
        return /;\s*charset=([^;\s]+)/i.exec(contentType)?.[1] ?? "utf-8";
    },

    fileIcon(contentType: string): string {
        const mimeType = contentType.split(";")[0];
        return ICON_BY_TYPE[mimeType] ?? ICON_BY_GROUP[mimeType.split("/")[0]] ?? "pi pi-file";
    },

    formatDate(iso: string, use24HourClock: boolean): string {
        return DATE_FORMATS[`${use24HourClock}`].format(new Date(iso));
    },

    // formatTimeLeft says how far off a time is, such as "in 23 hours". Under an hour it counts minutes.
    formatTimeLeft(iso: string): string {
        const minutes = Math.max(0, Math.round((new Date(iso).getTime() - Date.now()) / 60_000));
        if (minutes < 60) return RELATIVE_FORMAT.format(minutes, "minute");
        return RELATIVE_FORMAT.format(Math.round(minutes / 60), "hour");
    },

    baseNameLength(name: string, isFolder: boolean): number {
        const dot = name.lastIndexOf(".");
        return isFolder || dot <= 0 || dot === name.length - 1 ? name.length : dot;
    },
};

export default vueHelper;
