package dev.wanctl.agent;

import android.animation.ValueAnimator;
import android.content.Context;
import android.graphics.Canvas;
import android.graphics.Paint;
import android.graphics.Path;
import android.graphics.PathMeasure;
import android.graphics.RectF;
import android.view.MotionEvent;
import android.view.View;
import android.view.animation.DecelerateInterpolator;

/** The site's w mark, with motion tied to connection state. Idle states do not run a frame loop. */
final class ConnectionMark extends View {
    private final Paint paint = new Paint(Paint.ANTI_ALIAS_FLAG);
    private final Path mark = new Path();
    private final Path segment = new Path();
    private final PathMeasure measure;
    private AgentState.Phase phase;
    private boolean enabled, paused;
    private float cycle, reveal = 1, accent = 1;
    private ValueAnimator connecting, drawing, feedback;

    ConnectionMark(Context context) {
        super(context);
        // Same path as site/assets/mark.svg, including its optical vertical offset.
        mark.moveTo(5.6f, 13.7f);
        mark.lineTo(10.2f, 23.1f);
        mark.lineTo(15.2f, 15.9f);
        mark.lineTo(19.6f, 23.1f);
        mark.lineTo(26.4f, 8.9f);
        measure = new PathMeasure(mark, false);
        setClickable(true);
        setFocusable(true);
    }

    void setState(AgentState.Phase next, boolean running) {
        if (phase == next && enabled == running) return;
        boolean wasOnline = phase == AgentState.Phase.ONLINE;
        phase = next;
        enabled = running;
        stop();
        String status =
                phase == AgentState.Phase.ONLINE
                        ? "已连接"
                        : isConnecting()
                                ? "正在连接"
                                : phase == AgentState.Phase.ERROR ? "连接未完成" : "未启用";
        setContentDescription("wanctl，" + status);
        if (!paused && ValueAnimator.areAnimatorsEnabled()) {
            if (isConnecting()) startConnecting();
            else if (phase == AgentState.Phase.ONLINE && !wasOnline) {
                reveal = 0;
                drawing = ValueAnimator.ofFloat(0, 1);
                drawing.setDuration(550);
                drawing.setInterpolator(new DecelerateInterpolator());
                drawing.addUpdateListener(
                        a -> {
                            reveal = (float) a.getAnimatedValue();
                            invalidate();
                        });
                drawing.start();
                respond();
            }
        }
        invalidate();
    }

    private boolean isConnecting() {
        return enabled
                && (phase == AgentState.Phase.STARTING
                        || phase == AgentState.Phase.RETRYING
                        || phase == AgentState.Phase.STOPPED);
    }

    private void startConnecting() {
        if (connecting != null || !ValueAnimator.areAnimatorsEnabled()) return;
        connecting = ValueAnimator.ofFloat(0, 1);
        connecting.setDuration(1800);
        connecting.setRepeatCount(ValueAnimator.INFINITE);
        connecting.setInterpolator(new android.view.animation.LinearInterpolator());
        connecting.addUpdateListener(
                a -> {
                    cycle = (float) a.getAnimatedValue();
                    invalidate();
                });
        connecting.start();
    }

    private void respond() {
        if (!ValueAnimator.areAnimatorsEnabled() || paused) return;
        if (feedback != null) feedback.cancel();
        accent = 0;
        feedback = ValueAnimator.ofFloat(0, 1);
        feedback.setDuration(650);
        feedback.setInterpolator(new DecelerateInterpolator());
        feedback.addUpdateListener(
                a -> {
                    accent = (float) a.getAnimatedValue();
                    invalidate();
                });
        feedback.start();
    }

    @Override
    public boolean performClick() {
        super.performClick();
        respond();
        return true;
    }

    @Override
    public boolean onTouchEvent(MotionEvent event) {
        switch (event.getActionMasked()) {
            case MotionEvent.ACTION_DOWN:
                animate().cancel();
                if (ValueAnimator.areAnimatorsEnabled())
                    animate()
                            .scaleX(.96f)
                            .scaleY(.96f)
                            .setDuration(120)
                            .setInterpolator(new DecelerateInterpolator())
                            .start();
                return true;
            case MotionEvent.ACTION_UP:
                releasePress();
                if (event.getX() >= 0
                        && event.getX() <= getWidth()
                        && event.getY() >= 0
                        && event.getY() <= getHeight()) performClick();
                return true;
            case MotionEvent.ACTION_CANCEL:
                releasePress();
                return true;
            default:
                return true;
        }
    }

    private void releasePress() {
        if (ValueAnimator.areAnimatorsEnabled())
            animate()
                    .scaleX(1)
                    .scaleY(1)
                    .setDuration(180)
                    .setInterpolator(new DecelerateInterpolator())
                    .start();
        else {
            setScaleX(1);
            setScaleY(1);
        }
    }

    void pause() {
        paused = true;
        stop();
    }

    void resume() {
        paused = false;
        if (isConnecting()) startConnecting();
    }

    @Override
    protected void onAttachedToWindow() {
        super.onAttachedToWindow();
        resume();
    }

    @Override
    protected void onDetachedFromWindow() {
        pause();
        super.onDetachedFromWindow();
    }

    @Override
    protected void onWindowVisibilityChanged(int visibility) {
        super.onWindowVisibilityChanged(visibility);
        if (visibility == VISIBLE) resume();
        else pause();
    }

    private void stop() {
        if (connecting != null) connecting.cancel();
        if (drawing != null) drawing.cancel();
        if (feedback != null) feedback.cancel();
        connecting = drawing = feedback = null;
        reveal = accent = 1;
        animate().cancel();
        setScaleX(1);
        setScaleY(1);
    }

    @Override
    protected void onDraw(Canvas canvas) {
        super.onDraw(canvas);
        float scale = Math.min(getWidth(), getHeight()) / 128f;
        canvas.save();
        canvas.translate((getWidth() - 128 * scale) / 2, (getHeight() - 128 * scale) / 2);
        canvas.scale(scale, scale);
        boolean online = phase == AgentState.Phase.ONLINE;
        int ink = online || enabled ? 0xFF1D1D1F : 0xFFE9E9EE;
        if (accent < 1) {
            float spread = 2 + accent * 12;
            paint.setStyle(Paint.Style.STROKE);
            paint.setStrokeWidth(1.5f);
            paint.setColor(online ? 0xFF0066CC : 0xFF8E8E93);
            paint.setAlpha((int) (100 * (1 - accent)));
            canvas.drawRoundRect(
                    new RectF(16 - spread, 16 - spread, 112 + spread, 112 + spread),
                    24 + spread,
                    24 + spread,
                    paint);
        }
        paint.setAlpha(255);
        paint.setStyle(Paint.Style.FILL);
        paint.setColor(ink);
        float breath = isConnecting() ? 1 - .018f * (float) (1 - Math.cos(cycle * Math.PI * 2)) : 1;
        canvas.scale(breath, breath, 64, 64);
        canvas.drawRoundRect(new RectF(16, 16, 112, 112), 24, 24, paint);
        canvas.translate(16, 16);
        canvas.scale(3, 3);
        paint.setStyle(Paint.Style.STROKE);
        paint.setStrokeWidth(3.1f);
        paint.setStrokeCap(Paint.Cap.ROUND);
        paint.setStrokeJoin(Paint.Join.ROUND);
        paint.setColor(online ? 0xFF69696E : 0xFF8E8E93);
        canvas.drawPath(mark, paint);
        if (online || isConnecting()) {
            float length = measure.getLength();
            float from = 0, to = length * reveal;
            if (isConnecting()) {
                from = Math.max(0, cycle * length * 1.5f - length * .5f);
                to = Math.min(length, cycle * length * 1.5f);
            }
            segment.reset();
            measure.getSegment(from, to, segment, true);
            paint.setColor(0xFFFFFFFF);
            canvas.drawPath(segment, paint);
        }
        canvas.restore();
    }
}
