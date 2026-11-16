<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useTemplateRef, watch } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import { startDownload } from "../../../pkg/utils/download_link.ts";
import { isViewable, itemView, viewKind } from "../../../pkg/utils/item_view.ts";
import { useViewRoute } from "../../../pkg/utils/use_view_route.ts";
import { useZoomPan } from "../../../pkg/utils/use_zoom_pan.ts";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import type { ItemView, ViewKind } from "../../../pkg/utils/item_view.ts";
import type { StoredFile } from "../models.ts";

type ViewState = "loading" | "ready" | "failed";

interface Failure {
    icon: string;
    message: string;
}

// The text view reads at most this many bytes, so a big log never loads whole.
const TEXT_LIMIT = 1024 * 1024;

// What the viewer says when a file does not load. A PDF frame and the "other" view never fail.
const FAILURES: Partial<Record<ViewKind, Failure>> = {
    picture: { icon: "pi pi-image", message: "This picture cannot be shown." },
    video: { icon: "pi pi-video", message: "This video cannot be played here." },
    text: { icon: "pi pi-file", message: "This file cannot be shown." },
};

// items is the open page of the list. Next and previous step through its files in the list's order.
// The view param in the URL names the open file (see useViewRoute).
const props = defineProps<{ items: StoredFile[] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();
const { viewId, show, close } = useViewRoute();
const {
    transform,
    percent,
    zoomed,
    canZoomIn,
    dragging,
    zoomIn,
    zoomOut,
    reset,
    start,
    stop,
    onWheel,
    onDoubleClick,
    onPointerDown,
    onPointerMove,
    onPointerUp,
} = useZoomPan("stage", "picture");
const video = useTemplateRef<HTMLVideoElement>("video");

const viewable = computed<StoredFile[]>(() => props.items.filter(isViewable));
const listedIndex = computed<number>(() => viewable.value.findIndex((item) => item.id === viewId.value));
// A file that is not on the open page, such as after a reload on a later page, opens alone.
const single = ref<StoredFile | null>(null);
const shown = computed<StoredFile[]>(() => {
    if (listedIndex.value >= 0) return viewable.value;
    return single.value !== null && single.value.id === viewId.value ? [single.value] : [];
});
const activeIndex = computed<number>(() => Math.max(listedIndex.value, 0));
const current = computed<StoredFile | null>(() => shown.value[activeIndex.value] ?? null);
const kind = computed<ViewKind | null>(() => (current.value ? viewKind(current.value) : null));
const view = computed<ItemView | null>(() => (current.value ? itemView(current.value) : null));
const failure = computed<Failure | null>(() => (kind.value ? (FAILURES[kind.value] ?? null) : null));
const state = ref<ViewState>("loading");
const text = ref<string>("");
const sidePadding = computed<string>(() => (shown.value.length > 1 ? "px-14 sm:px-16" : "px-4"));

// An ID that is not a file of this user closes the viewer.
async function loadSingle(): Promise<void> {
    const id = viewId.value;
    if (id === null || listedIndex.value >= 0 || single.value?.id === id) return;
    try {
        const item = await fileStore.getItem(id);
        if (viewId.value !== id) return;
        if (isViewable(item)) {
            single.value = item;
            return;
        }
    } catch (error) {
        if (viewId.value !== id) return;
        toastStore.errorResponseToast(error);
    }
    await close();
}

async function step(index: number): Promise<void> {
    const item = shown.value[index];
    if (item) await show(item.id);
}

async function onVisibleChange(visible: boolean): Promise<void> {
    if (!visible) await close();
}

function download(): void {
    if (current.value) startDownload(fileStore.downloadUrl(current.value.id));
}

function onPictureLoad(): void {
    state.value = "ready";
    start();
}

// A browser that cannot decode the picture, such as HEVC in Chromium on Linux, gives no error. It plays the sound
// alone over a black box, so a video with no picture counts as failed. Pause it before autoplay starts.
function onVideoLoad(event: Event): void {
    const player = event.target as HTMLVideoElement;
    if (player.videoWidth === 0) {
        player.pause();
        onError();
        return;
    }
    state.value = "ready";
}

function onError(): void {
    state.value = "failed";
    stop();
}

// An empty file has no bytes to read. A cut file can end inside a character, and stream mode leaves that part out.
async function loadText(item: StoredFile): Promise<void> {
    try {
        const bytes = item.size_bytes > 0 ? await fileStore.readStart(item.id, TEXT_LIMIT) : new ArrayBuffer(0);
        if (current.value?.id !== item.id) return;
        text.value = new TextDecoder(vueHelper.charset(item.content_type)).decode(bytes, { stream: true });
        state.value = "ready";
    } catch {
        if (current.value?.id === item.id) onError();
    }
}

function togglePlay(): void {
    const player = video.value;
    if (!player) return;
    if (player.paused) void player.play();
    else player.pause();
}

// The arrow keys step, + - 0 zoom a picture, and space plays or pauses a video. A focused video keeps its own keys, so
// the arrow keys seek in it. Galleria closes the viewer on Escape.
function onKeydown(event: KeyboardEvent): void {
    if (current.value === null || event.ctrlKey || event.metaKey || event.altKey) return;
    if (event.target instanceof HTMLMediaElement) return;
    switch (event.key) {
        case "ArrowLeft":
            void step(activeIndex.value - 1);
            break;
        case "ArrowRight":
            void step(activeIndex.value + 1);
            break;
        case "+":
        case "=":
            if (kind.value !== "picture") return;
            zoomIn();
            break;
        case "-":
            if (kind.value !== "picture") return;
            zoomOut();
            break;
        case "0":
            if (kind.value !== "picture") return;
            reset();
            break;
        case " ":
            if (kind.value !== "video") return;
            togglePlay();
            break;
        default:
            return;
    }
    event.preventDefault();
}

watch(
    () => current.value?.id,
    () => {
        state.value = "loading";
        text.value = "";
        stop();
        if (current.value === null) return;
        if (kind.value === "text") void loadText(current.value);
        if (kind.value === "other") state.value = "ready";
    },
    { immediate: true },
);
watch([viewId, listedIndex], loadSingle, { immediate: true });

onMounted(() => window.addEventListener("keydown", onKeydown));
onBeforeUnmount(() => window.removeEventListener("keydown", onKeydown));
</script>

<template>
    <Galleria
        :visible="current !== null"
        :value="shown"
        :active-index="activeIndex"
        full-screen
        :show-thumbnails="false"
        :show-item-navigators="shown.length > 1"
        mask-class="[--px-mask-background:black]"
        container-class="flex h-dvh w-screen flex-col rounded-none border-0 bg-transparent"
        :pt="{
            content: 'flex min-h-0 flex-1 flex-col',
            itemsContainer: 'min-h-0 flex-1',
            items: 'h-full',
            item: 'h-full',
            prevButton: 'z-10',
            nextButton: 'z-10',
        }"
        @update:visible="onVisibleChange"
        @update:active-index="step"
    >
        <template #header>
            <div v-if="current" class="flex h-16 items-center gap-3 pr-16 pl-4 text-white sm:pl-6">
                <div class="flex min-w-0 flex-1 items-baseline gap-3">
                    <span class="truncate text-sm font-medium" :title="current.name">{{ current.name }}</span>
                    <span v-if="shown.length > 1" class="shrink-0 text-xs tabular-nums text-white/60">
                        {{ activeIndex + 1 }} of {{ shown.length }}
                    </span>
                </div>
                <div class="flex shrink-0 items-center gap-1">
                    <template v-if="kind === 'picture'">
                        <Button
                            icon="pi pi-search-minus"
                            size="small"
                            severity="secondary"
                            variant="text"
                            rounded
                            aria-label="Zoom out"
                            title="Zoom out (-)"
                            class="text-white hover:bg-white/10"
                            :disabled="!zoomed"
                            @click="zoomOut"
                        />
                        <Button
                            :label="state === 'ready' ? `${percent}%` : '–'"
                            size="small"
                            severity="secondary"
                            variant="text"
                            aria-label="Fit to screen"
                            title="Fit to screen (0)"
                            class="hidden min-w-16 text-white tabular-nums hover:bg-white/10 sm:inline-flex"
                            :disabled="state !== 'ready'"
                            @click="reset"
                        />
                        <Button
                            icon="pi pi-search-plus"
                            size="small"
                            severity="secondary"
                            variant="text"
                            rounded
                            aria-label="Zoom in"
                            title="Zoom in (+)"
                            class="text-white hover:bg-white/10"
                            :disabled="!canZoomIn"
                            @click="zoomIn"
                        />
                    </template>
                    <Button
                        icon="pi pi-download"
                        size="small"
                        severity="secondary"
                        variant="text"
                        rounded
                        aria-label="Download"
                        title="Download"
                        class="text-white hover:bg-white/10"
                        @click="download"
                    />
                </div>
            </div>
        </template>

        <template #item="{ item }">
            <div v-if="item" class="relative flex size-full items-center justify-center">
                <video
                    v-if="kind === 'video'"
                    :key="item.id"
                    ref="video"
                    :src="fileStore.contentUrl(item.id)"
                    controls
                    autoplay
                    playsinline
                    preload="metadata"
                    class="max-h-full max-w-full"
                    :class="{ invisible: state !== 'ready' }"
                    @loadedmetadata="onVideoLoad"
                    @error="onError"
                />
                <div
                    v-else-if="kind === 'picture'"
                    ref="stage"
                    class="flex size-full touch-none items-center justify-center overflow-hidden select-none"
                    :class="{
                        'cursor-zoom-in': !zoomed,
                        'cursor-grab': zoomed && !dragging,
                        'cursor-grabbing': dragging,
                    }"
                    @wheel.prevent="onWheel"
                    @dblclick="onDoubleClick"
                    @pointerdown="onPointerDown"
                    @pointermove="onPointerMove"
                    @pointerup="onPointerUp"
                    @pointercancel="onPointerUp"
                >
                    <img
                        :key="item.id"
                        ref="picture"
                        :src="fileStore.contentUrl(item.id)"
                        :alt="item.name"
                        draggable="false"
                        class="max-h-full max-w-full"
                        :class="{ 'transition-transform duration-150': !dragging, invisible: state !== 'ready' }"
                        :style="{ transform }"
                        @load="onPictureLoad"
                        @error="onError"
                    />
                </div>
                <div v-else-if="kind === 'pdf'" class="size-full pb-4" :class="sidePadding">
                    <iframe
                        :key="item.id"
                        :src="fileStore.contentUrl(item.id)"
                        :title="item.name"
                        class="size-full rounded-lg border-0 bg-white"
                        :class="{ invisible: state !== 'ready' }"
                        @load="state = 'ready'"
                    />
                </div>
                <!-- The text shows as plain text, so an HTML or XML file shows its source and never runs. -->
                <div v-else-if="kind === 'text'" class="flex size-full justify-center pb-4" :class="sidePadding">
                    <div
                        v-if="state === 'ready'"
                        class="flex w-full max-w-6xl flex-col overflow-hidden rounded-lg bg-panel text-fg"
                    >
                        <p
                            v-if="item.size_bytes > TEXT_LIMIT"
                            class="border-b border-edge px-4 py-2 text-xs text-fg-muted"
                        >
                            Shows the first {{ vueHelper.formatBytes(TEXT_LIMIT) }} of {{ view?.size }}. Download the
                            file to see all of it.
                        </p>
                        <p v-if="item.size_bytes === 0" class="p-4 text-sm text-fg-muted">This file is empty.</p>
                        <pre
                            v-else
                            class="min-h-0 flex-1 overflow-auto p-4 font-mono text-sm wrap-break-word whitespace-pre-wrap"
                            >{{ text }}</pre>
                    </div>
                </div>
                <div v-else class="flex flex-col items-center gap-3 px-4 text-center">
                    <i :class="view?.icon" class="text-5xl text-white/60" />
                    <span class="text-sm text-white">No preview for this file type.</span>
                    <span class="text-xs tabular-nums text-white/60">{{ view?.size }}</span>
                    <Button label="Download" icon="pi pi-download" size="small" @click="download" />
                </div>
                <i v-if="state === 'loading'" class="pi pi-spin pi-spinner absolute text-3xl text-white/70" />
                <div
                    v-if="state === 'failed' && failure"
                    class="absolute flex flex-col items-center gap-3 px-4 text-center"
                >
                    <i :class="failure.icon" class="text-3xl text-white/60" />
                    <span class="text-sm text-white">{{ failure.message }}</span>
                    <Button label="Download" icon="pi pi-download" size="small" @click="download" />
                </div>
            </div>
        </template>
    </Galleria>
</template>
