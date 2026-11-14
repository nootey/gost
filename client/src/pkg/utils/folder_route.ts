import type { RouteLocationRaw } from "vue-router";

// The URL names the open folder by its path, such as /?dir=/Photos/Cars.
// The root's dir is "", and its URL has no dir at all.

export function joinDir(dir: string, name: string): string {
    return `${dir}/${name}`;
}

export function folderRoute(dir: string): RouteLocationRaw {
    return dir === "" ? { name: "dashboard" } : { name: "dashboard", query: { dir } };
}
