import type { RouteLocationRaw } from "vue-router";

// The URL names the open folder by its path, such as /?dir=/Photos/Cars.
// The root's dir is "", and its URL has no dir at all.

export function joinDir(dir: string, name: string): string {
    return `${dir}/${name}`;
}

// The dir of a folder from its names, root first. No names is the root.
export function namesDir(names: string[]): string {
    return names.reduce((dir, name) => joinDir(dir, name), "");
}

// The dir as the user reads it, such as "Home / Photos / Cars".
export function dirLabel(dir: string): string {
    return ["Home", ...dir.split("/").filter(Boolean)].join(" / ");
}

export function folderRoute(dir: string): RouteLocationRaw {
    return dir === "" ? { name: "dashboard" } : { name: "dashboard", query: { dir } };
}
