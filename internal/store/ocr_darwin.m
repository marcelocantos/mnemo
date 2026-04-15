// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

#import <Foundation/Foundation.h>
#import <Vision/Vision.h>
#import <CoreImage/CoreImage.h>

#include "ocr_darwin.h"
#include <stdlib.h>
#include <string.h>

static char *copyNSString(NSString *s) {
    if (s == nil) {
        return NULL;
    }
    const char *cstr = [s UTF8String];
    if (cstr == NULL) {
        return NULL;
    }
    return strdup(cstr);
}

MnemoOCRResult mnemo_ocr_recognize(const void *data, size_t len) {
    MnemoOCRResult result = {NULL, 0.0, NULL};

    if (data == NULL || len == 0) {
        result.error = strdup("empty image data");
        return result;
    }

    @autoreleasepool {
        NSData *imageData = [NSData dataWithBytes:data length:(NSUInteger)len];
        CIImage *ciImage = [CIImage imageWithData:imageData];
        if (ciImage == nil) {
            result.error = strdup("could not decode image data");
            return result;
        }

        VNRecognizeTextRequest *request = [[VNRecognizeTextRequest alloc] init];
        request.recognitionLevel = VNRequestTextRecognitionLevelAccurate;
        request.usesLanguageCorrection = YES;

        VNImageRequestHandler *handler =
            [[VNImageRequestHandler alloc] initWithCIImage:ciImage options:@{}];

        NSError *error = nil;
        BOOL ok = [handler performRequests:@[request] error:&error];
        if (!ok || error != nil) {
            NSString *msg = error != nil
                ? [error localizedDescription]
                : @"VNImageRequestHandler.performRequests failed";
            result.error = copyNSString(msg);
            return result;
        }

        NSArray<VNRecognizedTextObservation *> *observations =
            (NSArray<VNRecognizedTextObservation *> *)request.results;

        NSMutableArray<NSString *> *lines =
            [NSMutableArray arrayWithCapacity:observations.count];
        double totalConfidence = 0.0;
        NSUInteger count = 0;

        for (VNRecognizedTextObservation *obs in observations) {
            VNRecognizedText *top = [[obs topCandidates:1] firstObject];
            if (top != nil) {
                [lines addObject:top.string];
                totalConfidence += (double)top.confidence;
                count++;
            }
        }

        NSString *joined = [lines componentsJoinedByString:@"\n"];
        result.text = copyNSString(joined);
        if (count > 0) {
            result.confidence = totalConfidence / (double)count;
        }
    }

    return result;
}

void mnemo_ocr_free_result(MnemoOCRResult *r) {
    if (r == NULL) {
        return;
    }
    if (r->text != NULL) {
        free(r->text);
        r->text = NULL;
    }
    if (r->error != NULL) {
        free(r->error);
        r->error = NULL;
    }
}
