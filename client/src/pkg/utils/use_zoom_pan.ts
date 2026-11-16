import { computed, onBeforeUnmount, onMounted, ref, useTemplateRef } from "vue";
import type { ComputedRef, Ref } from "vue";

const ZOOM_STEP = 1.25;
// The most zoom shows each picture pixel as 4 screen pixels.
const MAX_PIXEL_ZOOM = 4;
// A wheel turn of 100 pixels zooms by about 1.16. One event moves 100 pixels at most, so a fast wheel cannot jump.
const WHEEL_SPEED = 0.0015;
const MAX_WHEEL_DELTA = 100;
// A wheel that counts lines or pages moves this many pixels for each one.
const LINE_PIXELS = 16;

interface Point {
    x: number;
    y: number;
}

interface Drag {
    pointerId: number;
    start: Point;
    origin: Point;
}

export interface ZoomPan {
    transform: ComputedRef<string>;
    percent: ComputedRef<number>;
    zoomed: ComputedRef<boolean>;
    canZoomIn: ComputedRef<boolean>;
    dragging: Ref<boolean>;
    zoomIn: () => void;
    zoomOut: () => void;
    reset: () => void;
    start: () => void;
    stop: () => void;
    onWheel: (event: WheelEvent) => void;
    onDoubleClick: (event: MouseEvent) => void;
    onPointerDown: (event: PointerEvent) => void;
    onPointerMove: (event: PointerEvent) => void;
    onPointerUp: (event: PointerEvent) => void;
}

// stageKey and imageKey are template refs. The image sits in the middle of the stage at its fitted size, which is
// scale 1. A zoom keeps the point under the pointer in place. A drag moves a zoomed image, but a side of the image
// never comes inside the stage. Call start when the image loads, and stop before another image loads or after one
// fails. Until start, nothing zooms.
export function useZoomPan(stageKey: string, imageKey: string): ZoomPan {
    const stage = useTemplateRef<HTMLElement>(stageKey);
    const image = useTemplateRef<HTMLImageElement>(imageKey);

    const scale = ref<number>(1);
    const offset = ref<Point>({ x: 0, y: 0 });
    // The scale that shows each picture pixel as one CSS pixel. A picture that fits at its own size has 1.
    const pixelScale = ref<number>(1);
    const ready = ref<boolean>(false);
    const dragging = ref<boolean>(false);
    let drag: Drag | null = null;

    const maxScale = computed<number>(() => pixelScale.value * MAX_PIXEL_ZOOM);
    const transform = computed<string>(
        () => `translate(${offset.value.x}px, ${offset.value.y}px) scale(${scale.value})`,
    );
    // 100 is the picture's own size.
    const percent = computed<number>(() => Math.round((scale.value / pixelScale.value) * 100));
    const zoomed = computed<boolean>(() => scale.value > 1);
    const canZoomIn = computed<boolean>(() => ready.value && scale.value < maxScale.value);

    function clamp(point: Point): Point {
        const img = image.value;
        const box = stage.value;
        if (!img || !box) return { x: 0, y: 0 };
        const limitX = Math.max(0, (img.offsetWidth * scale.value - box.clientWidth) / 2);
        const limitY = Math.max(0, (img.offsetHeight * scale.value - box.clientHeight) / 2);
        return {
            x: Math.min(Math.max(point.x, -limitX), limitX),
            y: Math.min(Math.max(point.y, -limitY), limitY),
        };
    }

    // at is a point in the stage, from its middle.
    function zoomTo(next: number, at: Point = { x: 0, y: 0 }): void {
        if (!ready.value) return;
        const target = Math.min(Math.max(next, 1), maxScale.value);
        const ratio = target / scale.value;
        const from = offset.value;
        scale.value = target;
        offset.value = clamp({ x: at.x - ratio * (at.x - from.x), y: at.y - ratio * (at.y - from.y) });
    }

    function stagePoint(event: MouseEvent): Point {
        const box = stage.value?.getBoundingClientRect();
        if (!box) return { x: 0, y: 0 };
        return { x: event.clientX - box.left - box.width / 2, y: event.clientY - box.top - box.height / 2 };
    }

    function zoomIn(): void {
        zoomTo(scale.value * ZOOM_STEP);
    }

    function zoomOut(): void {
        zoomTo(scale.value / ZOOM_STEP);
    }

    function reset(): void {
        scale.value = 1;
        offset.value = { x: 0, y: 0 };
    }

    // The fitted size changes with the window, so the zoom limits change too.
    function measure(): void {
        if (!ready.value) return;
        const img = image.value;
        pixelScale.value = img && img.offsetWidth > 0 ? Math.max(1, img.naturalWidth / img.offsetWidth) : 1;
        zoomTo(scale.value);
    }

    function start(): void {
        ready.value = true;
        measure();
    }

    function stop(): void {
        ready.value = false;
        pixelScale.value = 1;
        endDrag();
        reset();
    }

    function onWheel(event: WheelEvent): void {
        const unit = event.deltaMode === WheelEvent.DOM_DELTA_PIXEL ? 1 : LINE_PIXELS;
        const delta = Math.min(Math.max(event.deltaY * unit, -MAX_WHEEL_DELTA), MAX_WHEEL_DELTA);
        zoomTo(scale.value * Math.exp(-delta * WHEEL_SPEED), stagePoint(event));
    }

    // A double click shows the picture's own pixels at the pointer, or twice the size for a small picture. On a zoomed
    // picture, it fits the picture again.
    function onDoubleClick(event: MouseEvent): void {
        if (!ready.value) return;
        if (zoomed.value) {
            reset();
            return;
        }
        zoomTo(Math.max(pixelScale.value, 2), stagePoint(event));
    }

    function onPointerDown(event: PointerEvent): void {
        if (!zoomed.value || event.button !== 0 || drag) return;
        stage.value?.setPointerCapture(event.pointerId);
        drag = { pointerId: event.pointerId, start: { x: event.clientX, y: event.clientY }, origin: offset.value };
        dragging.value = true;
    }

    function onPointerMove(event: PointerEvent): void {
        const current = drag;
        if (!current || current.pointerId !== event.pointerId) return;
        offset.value = clamp({
            x: current.origin.x + event.clientX - current.start.x,
            y: current.origin.y + event.clientY - current.start.y,
        });
    }

    function onPointerUp(event: PointerEvent): void {
        if (drag?.pointerId === event.pointerId) endDrag();
    }

    function endDrag(): void {
        drag = null;
        dragging.value = false;
    }

    onMounted(() => window.addEventListener("resize", measure));
    onBeforeUnmount(() => window.removeEventListener("resize", measure));

    return {
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
    };
}
