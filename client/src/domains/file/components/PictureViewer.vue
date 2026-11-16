<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from "vue";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import { startDownload } from "../../../pkg/utils/download_link.ts";
import { isPicture } from "../../../pkg/utils/item_view.ts";
import { useViewRoute } from "../../../pkg/utils/use_view_route.ts";
import { useZoomPan } from "../../../pkg/utils/use_zoom_pan.ts";
import type { StoredFile } from "../models.ts";

type PictureState = "loading" | "ready" | "failed";

// items is the open page of the list. Next and previous step through its pictures in the list's order. The view param
// in the URL names the open picture (see useViewRoute).
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

const pictures = computed<StoredFile[]>(() => props.items.filter(isPicture));
const listedIndex = computed<number>(() => pictures.value.findIndex((item) => item.id === viewId.value));
// A picture that is not on the open page, such as after a reload on a later page, opens alone.
const single = ref<StoredFile | null>(null);
const shown = computed<StoredFile[]>(() => {
    if (listedIndex.value >= 0) return pictures.value;
    return single.value !== null && single.value.id === viewId.value ? [single.value] : [];
});
const activeIndex = computed<number>(() => Math.max(listedIndex.value, 0));
const current = computed<StoredFile | null>(() => shown.value[activeIndex.value] ?? null);
const state = ref<PictureState>("loading");

// An ID that is not a picture of this user closes the viewer.
async function loadSingle(): Promise<void> {
    const id = viewId.value;
    if (id === null || listedIndex.value >= 0 || single.value?.id === id) return;
    try {
        const item = await fileStore.getItem(id);
        if (viewId.value !== id) return;
        if (isPicture(item)) {
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

function onLoad(): void {
    state.value = "ready";
    start();
}

function onError(): void {
    state.value = "failed";
    stop();
}

// The arrow keys step, and + - 0 zoom. Galleria closes the viewer on Escape.
function onKeydown(event: KeyboardEvent): void {
    if (current.value === null || event.ctrlKey || event.metaKey || event.altKey) return;
    switch (event.key) {
        case "ArrowLeft":
            void step(activeIndex.value - 1);
            break;
        case "ArrowRight":
            void step(activeIndex.value + 1);
            break;
        case "+":
        case "=":
            zoomIn();
            break;
        case "-":
            zoomOut();
            break;
        case "0":
            reset();
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
        stop();
    },
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
            <div
                v-if="item"
                ref="stage"
                class="relative flex size-full touch-none items-center justify-center overflow-hidden select-none"
                :class="{ 'cursor-zoom-in': !zoomed, 'cursor-grab': zoomed && !dragging, 'cursor-grabbing': dragging }"
                @wheel.prevent="onWheel"
                @dblclick="onDoubleClick"
                @pointerdown="onPointerDown"
                @pointermove="onPointerMove"
                @pointerup="onPointerUp"
                @pointercancel="onPointerUp"
            >
                <!-- An <img> ignores the download's Content-Disposition, so the download URL shows the stored bytes. -->
                <img
                    :key="item.id"
                    ref="picture"
                    :src="fileStore.downloadUrl(item.id)"
                    :alt="item.name"
                    draggable="false"
                    class="max-h-full max-w-full"
                    :class="{ 'transition-transform duration-150': !dragging, invisible: state !== 'ready' }"
                    :style="{ transform }"
                    @load="onLoad"
                    @error="onError"
                />
                <i v-if="state === 'loading'" class="pi pi-spin pi-spinner absolute text-3xl text-white/70" />
                <div v-if="state === 'failed'" class="absolute flex flex-col items-center gap-3 px-4 text-center">
                    <i class="pi pi-image text-3xl text-white/60" />
                    <span class="text-sm text-white">This picture cannot be shown.</span>
                    <Button label="Download" icon="pi pi-download" size="small" @click="download" />
                </div>
            </div>
        </template>
    </Galleria>
</template>
