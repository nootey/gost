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

    fileIcon(contentType: string): string {
        const mimeType = contentType.split(";")[0];
        return ICON_BY_TYPE[mimeType] ?? ICON_BY_GROUP[mimeType.split("/")[0]] ?? "pi pi-file";
    },
};

export default vueHelper;
