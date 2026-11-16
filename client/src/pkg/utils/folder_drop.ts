// A file of a drop or a chosen folder, with the path of its folder in the drop. dir is "" on top.
export interface DroppedFile {
    file: File;
    dir: string;
}

// What a drop or a chosen folder holds. dirs has the path of every folder in it, parents first.
export interface DroppedTree {
    files: DroppedFile[];
    dirs: string[];
}

// readDrop reads the files and folders of a drop, empty folders too. The browser lets the caller read the drop's
// items only while the drop event runs, so the entries are taken before the first await. An item with no entry,
// such as one from a drop made by a script, goes in as a plain file on top.
export function readDrop(items: DataTransferItemList): Promise<DroppedTree> {
    const entries: FileSystemEntry[] = [];
    const loose: DroppedFile[] = [];
    for (const item of [...items]) {
        if (item.kind !== "file") continue;
        const entry = item.webkitGetAsEntry();
        const file = entry ? null : item.getAsFile();
        if (entry) entries.push(entry);
        else if (file) loose.push({ file, dir: "" });
    }
    return readEntries(entries, loose);
}

// treeFromPaths builds the tree of a chosen folder from the path the browser gives each file. The browser leaves
// empty folders out.
export function treeFromPaths(files: File[]): DroppedTree {
    const dirs = new Set<string>();
    const dropped = files.map((file) => {
        const names = file.webkitRelativePath.split("/").slice(0, -1);
        names.forEach((_, i) => dirs.add(names.slice(0, i + 1).join("/")));
        return { file, dir: names.join("/") };
    });
    return { files: dropped, dirs: [...dirs] };
}

async function readEntries(entries: FileSystemEntry[], loose: DroppedFile[]): Promise<DroppedTree> {
    const tree: DroppedTree = { files: [...loose], dirs: [] };

    const visit = async (entry: FileSystemEntry, dir: string): Promise<void> => {
        if (entry.isFile) {
            const file = await new Promise<File>((resolve, reject) =>
                (entry as FileSystemFileEntry).file(resolve, reject),
            );
            tree.files.push({ file, dir });
            return;
        }
        if (!entry.isDirectory) return;

        const path = dir === "" ? entry.name : `${dir}/${entry.name}`;
        tree.dirs.push(path);
        for (const child of await childrenOf(entry as FileSystemDirectoryEntry)) {
            await visit(child, path);
        }
    };

    for (const entry of entries) {
        await visit(entry, "");
    }
    return tree;
}

// childrenOf reads all entries of a folder. One readEntries call gives at most about 100, so it runs until it gives
// none.
async function childrenOf(dir: FileSystemDirectoryEntry): Promise<FileSystemEntry[]> {
    const reader = dir.createReader();
    const all: FileSystemEntry[] = [];
    for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
        if (batch.length === 0) return all;
        all.push(...batch);
    }
}
